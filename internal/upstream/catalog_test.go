package upstream

import (
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestCatalogStoreAndLookupRealmIsolation 全量目录按 realm 分层桶，互不可见。
func TestCatalogStoreAndLookupRealmIsolation(t *testing.T) {
	c := &Client{}
	if _, ok := c.CatalogLookup("cn", "a"); ok {
		t.Fatal("空 catalog 不应命中")
	}
	c.storeCatalog("cn", []ModelInfo{{ID: "a", ContextWindow: 100, Name: "A"}})
	mi, ok := c.CatalogLookup("cn", "a")
	if !ok || mi.ContextWindow != 100 || mi.Name != "A" {
		t.Fatalf("cn:a 应命中且字段完整，got %+v ok=%v", mi, ok)
	}
	if _, ok := c.CatalogLookup("global", "a"); ok {
		t.Fatal("realm 桶必须隔离：global 不应看到 cn 条目")
	}
}

// TestCatalogEmptyDoesNotClear 空列表不写：避免"某次探测没给全"清掉既有快照。
func TestCatalogEmptyDoesNotClear(t *testing.T) {
	c := &Client{}
	c.storeCatalog("cn", []ModelInfo{{ID: "a"}})
	c.storeCatalog("cn", nil)
	c.storeCatalog("cn", []ModelInfo{})
	if _, ok := c.CatalogLookup("cn", "a"); !ok {
		t.Fatal("空列表不应清掉既有快照")
	}
}

// TestCatalogSnapshotIsCopy Snapshot 返回副本（改它不影响内部快照）；未探测 → nil。
func TestCatalogSnapshotIsCopy(t *testing.T) {
	c := &Client{}
	c.storeCatalog("cn", []ModelInfo{{ID: "a", ContextWindow: 1}})
	snap := c.CatalogSnapshot("cn")
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d，want 1", len(snap))
	}
	snap["a"] = ModelInfo{ID: "a", ContextWindow: 999}
	if mi, _ := c.CatalogLookup("cn", "a"); mi.ContextWindow != 1 {
		t.Fatal("Snapshot 必须是副本，不得暴露内部 map")
	}
	if c.CatalogSnapshot("global") != nil {
		t.Fatal("未探测过的 realm 应返回 nil")
	}
}

// TestCatalogFromDyn 跳过空 ID，富字段按 dynEntry→ModelInfo 单一映射转换。
func TestCatalogFromDyn(t *testing.T) {
	got := catalogFromDyn(map[string]dynModelEntry{
		"a": {ID: "a", MaxInputTokens: 10, MaxOutputTokens: 5, Credits: "x0.1", Vendor: "e"},
		"b": {ID: ""},
	})
	if len(got) != 1 {
		t.Fatalf("空 ID 应被跳过，got %d 条", len(got))
	}
	if got[0].ID != "a" || got[0].ContextWindow != 10 || got[0].MaxTokens != 5 ||
		got[0].Credits != "x0.1" || got[0].Vendor != "e" {
		t.Fatalf("字段映射错误: %+v", got[0])
	}
}

// TestFetchModelsCatalogKeepsWhitelistFilteredEntries 核心回归测试：
// 企业端点 data.models 里的条目若不在 agents[cli] 白名单，动态目录不返回它，
// 但**必须**留在 catalog 里 —— 否则 config.models.extra_cn 的补充项拿不到
// name/context/maxOutputTokens，context_length 会落到 1M 兜底值。
func TestFetchModelsCatalogKeepsWhitelistFilteredEntries(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"in-cli","name":"In","maxInputTokens":1000,"maxOutputTokens":500},
				{"id":"out-cli","name":"Out","maxInputTokens":96000,"maxOutputTokens":32000,
				 "credits":"x0.29","vendor":"e","supportsReasoning":true,"onlyReasoning":true}
			],"agents":[{"name":"cli","models":["in-cli"]}]}}`), nil
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			return jsonResp(200, `{"code":0,"data":{"models":[]}}`), nil
		default:
			return jsonResp(404, `{}`), nil
		}
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	infos, err := c.FetchModels(a)
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(infos) != 1 || infos[0].ID != "in-cli" {
		t.Fatalf("动态目录应只含白名单内的条目，got %+v", infos)
	}
	mi, ok := c.CatalogLookup(a.Realm(), "out-cli")
	if !ok {
		t.Fatalf("被白名单挡掉的 out-cli 必须留在 catalog（realm=%q）", a.Realm())
	}
	if mi.ContextWindow != 96000 || mi.MaxTokens != 32000 || mi.Credits != "x0.29" ||
		mi.Vendor != "e" || mi.Name != "Out" || !mi.SupportsReasoning || !mi.OnlyReasoning {
		t.Fatalf("catalog 里的元数据不完整: %+v", mi)
	}
	// 白名单内的条目同样入 catalog（补充项可能与动态项同名，查表口径要一致）
	if _, ok := c.CatalogLookup(a.Realm(), "in-cli"); !ok {
		t.Fatal("in-cli 也应入 catalog")
	}
}

// TestFetchModelsCatalogSkipsNonChat 非对话模型不进 catalog（与动态目录同口径）。
func TestFetchModelsCatalogSkipsNonChat(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"glm-5.2","name":"GLM","maxInputTokens":131072,"maxOutputTokens":8192},
				{"id":"nes-1.1","name":"NES","maxInputTokens":1000,"maxOutputTokens":8000}
			],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`), nil
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			return jsonResp(200, `{"code":0,"data":{"models":[]}}`), nil
		default:
			return jsonResp(404, `{}`), nil
		}
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	if _, err := c.FetchModels(a); err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if _, ok := c.CatalogLookup(a.Realm(), "nes-1.1"); ok {
		t.Fatal("nes- 前缀的非对话模型不应进 catalog")
	}
}

// TestCatalogOrSeedThreeLevels 补充项元数据三级查找（全仓唯一实现）：
//
//  1. 本 realm 全量目录 → 2. CN 同名回落（仅 global）→ 3. 静态种子表 → 4. 裸 ID
//
// 为什么值得锁：handler 与 panel 曾各写一份等价逻辑，round6 给 handler 加第三级时
// 漏改 panel，导致 /v1/models 与面板「模型与档位」对同一模型给出不同字段。
// 抽成单一实现后，这个测试就是防漂移的锚点——两侧共用它，改一处即两侧同步。
func TestCatalogOrSeedThreeLevels(t *testing.T) {
	c := &Client{}
	c.storeCatalog("cn", []ModelInfo{{ID: "cn-only", Name: "CN 专有", Credits: "x0.10"}})
	c.storeCatalog("global", []ModelInfo{{ID: "gl-both", Name: "Global 有"}})

	// 第 1 级：本 realm 目录命中
	if mi, ok := c.CatalogOrSeed("global", "gl-both"); !ok || mi.Name != "Global 有" {
		t.Errorf("L1 本 realm 命中失败: %+v ok=%v", mi, ok)
	}
	// 第 2 级：global 目录 miss → 回落 CN 同名条目
	if mi, ok := c.CatalogOrSeed("global", "cn-only"); !ok || mi.Name != "CN 专有" || mi.Credits != "x0.10" {
		t.Errorf("L2 CN 回落失败: %+v ok=%v", mi, ok)
	}
	// 第 2 级**不**对 cn 域开：cn 侧就是 CN，不存在"回落 CN"
	if _, ok := c.CatalogOrSeed("cn", "gl-both"); ok {
		t.Error("cn 域不应看到 global 目录条目（realm 桶必须隔离）")
	}
	// 第 3 级：两级全 miss → 静态种子
	if mi, ok := c.CatalogOrSeed("global", "hy4-preview-f"); !ok || mi.Credits != "x0.00" || !mi.SupportsToolCall {
		t.Errorf("L3 静态种子失败: %+v ok=%v", mi, ok)
	}
	// 第 3 级按 realm 分表：cn 域不得命中 global 种子
	if _, ok := c.CatalogOrSeed("cn", "hy4-preview-f"); ok {
		t.Error("cn 域不应命中 global 种子（credits 按域定价，跨域复用会写错倍率）")
	}
	// 第 4 级：三级全 miss → false，调用方退化为裸 ID（不编造字段）
	if _, ok := c.CatalogOrSeed("global", "no-such-xyz"); ok {
		t.Error("三级全 miss 应返回 false")
	}
}

// TestCatalogOrSeedAlignsID 返回条目的 ID 必须对齐调用方传入的 id
// （catalog 以 id 为键；种子表里的键也是 id，但防御性重写避免未来改键出错）。
func TestCatalogOrSeedAlignsID(t *testing.T) {
	c := &Client{}
	c.storeCatalog("cn", []ModelInfo{{ID: "a", Name: "A"}})
	for _, tc := range []struct{ realm, id string }{
		{"cn", "a"}, {"global", "hy4-preview-f"},
	} {
		mi, ok := c.CatalogOrSeed(tc.realm, tc.id)
		if !ok {
			t.Fatalf("%s:%s 应命中", tc.realm, tc.id)
		}
		if mi.ID != tc.id {
			t.Errorf("%s:%s 返回 ID=%q 未对齐", tc.realm, tc.id, mi.ID)
		}
	}
}

// TestCatalogOrSeedEmptyID 空 id 一律 miss（与 CatalogLookup 同口径），
// 防止调用方传空串时误命中种子表里的零值键。
func TestCatalogOrSeedEmptyID(t *testing.T) {
	c := &Client{}
	if _, ok := c.CatalogOrSeed("global", ""); ok {
		t.Error("空 id 不应命中")
	}
}
