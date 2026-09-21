package server

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// newSeedTestHandler 构造一个 catalog 全空的 handler（upstream.New() 未探测过 ⇒
// CatalogLookup 两级全 miss），用于验证「catalog → CN 回落 → 静态种子 → 裸 ID」四级语义。
func newSeedTestHandler() *Handler {
	return NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
	})
}

// TestCatalogOrSeedFallsBackToStaticSeed 补充项元数据三级查找的第三级：
// catalog 两级全 miss 时由 upstream.ExtraModelSeed 兜底，而不是退化成裸 ID。
//
// 为什么值得锁：round5 把 hy4-preview-f 加进 extra_global 后，/v1/models 里它只有
// 6 个字段（缺 name/credits/supports_tool_call/max_output_tokens 等 11 个）——
// 客户端据此渲染选择器或决定是否发 tools 时会当成"不支持工具调用、输出上限未知"。
func TestCatalogOrSeedFallsBackToStaticSeed(t *testing.T) {
	h := newSeedTestHandler()

	mi, ok := h.catalogOrSeed("global", "hy4-preview-f")
	if !ok {
		t.Fatal("global:hy4-preview-f 应由静态种子命中")
	}
	if mi.ID != "hy4-preview-f" {
		t.Errorf("ID=%q want hy4-preview-f", mi.ID)
	}
	// 逐项对应 /v1/models 的透出字段（applyModelInfoFields 的输入）
	if mi.Name == "" || mi.Credits == "" || mi.Description == "" || mi.Vendor == "" {
		t.Errorf("种子字段不完整: name=%q credits=%q desc=%q vendor=%q",
			mi.Name, mi.Credits, mi.Description, mi.Vendor)
	}
	if !mi.SupportsToolCall || !mi.SupportsReasoning || !mi.SupportsImages {
		t.Errorf("能力旗标缺失: tool=%v reasoning=%v images=%v",
			mi.SupportsToolCall, mi.SupportsReasoning, mi.SupportsImages)
	}
	if mi.MaxTokens <= 0 || mi.MaxAllowedSize <= 0 {
		t.Errorf("maxOutput=%d maxAllowed=%d，客户端会拿不到 max_output_tokens", mi.MaxTokens, mi.MaxAllowedSize)
	}
}

// TestCatalogOrSeedRealmScoped CN 域不得命中 global 种子：credits 按域定价，
// 跨域复用会写出错误倍率。
func TestCatalogOrSeedRealmScoped(t *testing.T) {
	h := newSeedTestHandler()
	if _, ok := h.catalogOrSeed("cn", "hy4-preview-f"); ok {
		t.Error("cn 域不应命中 hy4-preview-f 种子")
	}
}

// TestCatalogOrSeedMissStaysBare 三级全 miss → 返回 false，调用方退化为裸 ID 条目，
// **不编造字段**（这是既有产品决策，别改成填默认值）。
func TestCatalogOrSeedMissStaysBare(t *testing.T) {
	h := newSeedTestHandler()
	if _, ok := h.catalogOrSeed("global", "no-such-model-xyz"); ok {
		t.Error("未收录 id 不应命中")
	}
	mi := h.fillFromCatalog("global", "no-such-model-xyz")
	if mi.ID != "no-such-model-xyz" {
		t.Errorf("ID=%q want no-such-model-xyz（裸 ID 条目仍需保留 id）", mi.ID)
	}
	if mi.Name != "" || mi.Credits != "" || mi.Description != "" ||
		mi.ContextWindow != 0 || mi.MaxTokens != 0 || mi.SupportsToolCall {
		t.Errorf("裸 ID 条目不得编造字段: %+v", mi)
	}
}

// TestFillFromCatalogUsesSeed CN 动态分支的补充项回填同样吃到第三级种子
// （两条分支共用 catalogOrSeed，防止只改一处导致语义漂移）。
func TestFillFromCatalogUsesSeed(t *testing.T) {
	h := newSeedTestHandler()
	// CN 域不命中 global 种子 ⇒ 裸 ID；这同时证明 fillFromCatalog 确实走了 catalogOrSeed。
	mi := h.fillFromCatalog("cn", "hy4-preview-f")
	if mi.Name != "" || mi.Credits != "" {
		t.Errorf("cn 域应退化为裸 ID，实际: %+v", mi)
	}
}
