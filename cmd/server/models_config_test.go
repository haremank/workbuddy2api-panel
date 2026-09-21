package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 2026-09-21 加：models 补充/屏蔽段的配置链路守护。
//
// 为什么值得单测：该段的值在装配期被注入 handler/panel（值拷贝），
// 而面板「配置」页提交的是**部分表单**（只含 CFG_MAP 里的键，不含 models）。
// 若 mergeConfigMaps 的深合并语义被改坏，用户任何一次配置保存都会静默吞掉
// 补充名单 —— 表现为"5 个可用模型突然从 /v1/models 消失"，且没有任何报错。

const modelsConfigJSON = `{
  "listen": ":7863",
  "api_key": "k",
  "models": {
    "extra_cn": ["deepseek-v3-2-volc", "kimi-k2.5"],
    "extra_global": [],
    "hide_cn": [],
    "hide_global": ["o4-mini", "auto-chat"]
  }
}`

func TestModelsConfigParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(modelsConfigJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Models.ExtraCN; len(got) != 2 || got[0] != "deepseek-v3-2-volc" || got[1] != "kimi-k2.5" {
		t.Errorf("extra_cn=%v", got)
	}
	if got := c.Models.HideGlobal; len(got) != 2 || got[0] != "o4-mini" || got[1] != "auto-chat" {
		t.Errorf("hide_global=%v", got)
	}
	if len(c.Models.ExtraGlobal) != 0 || len(c.Models.HideCN) != 0 {
		t.Errorf("空数组应保持为空：extra_global=%v hide_cn=%v", c.Models.ExtraGlobal, c.Models.HideCN)
	}
}

func TestModelsConfigAbsentIsZero(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"listen":":7863","api_key":"k"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	// 缺省 = 全空 = 完全不动（对老配置零影响）。
	if len(c.Models.ExtraCN)+len(c.Models.ExtraGlobal)+len(c.Models.HideCN)+len(c.Models.HideGlobal) != 0 {
		t.Errorf("缺省应为全空，得到 %+v", c.Models)
	}
}

// 面板保存配置时提交的是部分表单：models 不在其中，必须原样保留。
func TestMergeConfigMapsKeepsModelsOnPanelSave(t *testing.T) {
	var cur, incoming map[string]any
	if err := json.Unmarshal([]byte(modelsConfigJSON), &cur); err != nil {
		t.Fatal(err)
	}
	// 模拟面板表单提交：只有 CFG_MAP 覆盖的键，且含嵌套对象 pool。
	panelForm := `{"listen":":7863","api_key":"k2","pool":{"max_in_flight":4}}`
	if err := json.Unmarshal([]byte(panelForm), &incoming); err != nil {
		t.Fatal(err)
	}
	merged := mergeConfigMaps(cur, incoming)

	m, ok := merged["models"].(map[string]any)
	if !ok {
		t.Fatalf("models 段被面板保存吞掉了：merged=%v", merged)
	}
	extra, _ := m["extra_cn"].([]any)
	if len(extra) != 2 {
		t.Fatalf("extra_cn 丢失：%v", m["extra_cn"])
	}
	hide, _ := m["hide_global"].([]any)
	if len(hide) != 2 {
		t.Fatalf("hide_global 丢失：%v", m["hide_global"])
	}
	// 提交的键要生效，且不得影响兄弟键。
	if merged["api_key"] != "k2" {
		t.Errorf("api_key 未更新：%v", merged["api_key"])
	}
	if _, ok := merged["pool"].(map[string]any); !ok {
		t.Errorf("pool 段异常：%v", merged["pool"])
	}
	// 合并结果仍须能通过校验。
	if _, err := ParseConfig(mergedJSON(merged)); err != nil {
		t.Fatalf("合并后配置校验失败：%v", err)
	}
}

// 用户手写的未知键同样不能被面板保存吞掉（既有语义，顺带守护）。
func TestMergeConfigMapsKeepsUnknownUserKeys(t *testing.T) {
	cur := map[string]any{"listen": ":7863", "_my_note": "手写备注"}
	incoming := map[string]any{"api_key": "k"}
	merged := mergeConfigMaps(cur, incoming)
	if merged["_my_note"] != "手写备注" {
		t.Errorf("手写键被吞：%v", merged)
	}
}

func TestRestartRequiredIncludesModels(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(modelsConfigJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range restartRequiredFields(c) {
		if f == "models" {
			found = true
		}
	}
	if !found {
		t.Error("配了 models 段时 restart_required 应含 models（值在装配期注入，改后需重启）")
	}

	// 全空 = 不改动 → 不该出现在重启清单里（否则对没配的用户是噪音）。
	empty := Default()
	for _, f := range restartRequiredFields(empty) {
		if f == "models" {
			t.Error("models 段为空时不应出现在 restart_required 里")
		}
	}
}
