package upstream

import "testing"

// TestExtraModelSeedHy4PreviewF 锁定 hy4-preview-f 的种子字段。
//
// 为什么值得锁：这些值决定 /v1/models 里该条目是否**可用**——缺 supports_tool_call
// 会让客户端把模型当"不支持工具调用"而拒绝发 tools；缺 max_output_tokens 会让客户端
// 按自己的默认值发超长 max_tokens（上游 64000 上限 → 400）。值取自官方客户端产品配置
// （见 extra_seed.go 头注），不是编造，故应逐字段锁定，防止被顺手改成估算值。
func TestExtraModelSeedHy4PreviewF(t *testing.T) {
	mi, ok := ExtraModelSeed("global", "hy4-preview-f")
	if !ok {
		t.Fatal("global:hy4-preview-f 应有静态种子")
	}
	if mi.ID != "hy4-preview-f" {
		t.Errorf("ID=%q want hy4-preview-f（应以调用方 id 对齐）", mi.ID)
	}
	if mi.Name != "Hy4 preview" {
		t.Errorf("Name=%q", mi.Name)
	}
	if mi.Credits != "x0.00" {
		t.Errorf("Credits=%q want x0.00（免费档；同族 hy4-preview 是 x0.29）", mi.Credits)
	}
	if mi.ContextWindow != 1000000 || mi.MaxTokens != 64000 || mi.MaxAllowedSize != 1000000 {
		t.Errorf("ctx/maxOut/maxAllowed = %d/%d/%d want 1000000/64000/1000000",
			mi.ContextWindow, mi.MaxTokens, mi.MaxAllowedSize)
	}
	if !mi.SupportsToolCall || !mi.SupportsReasoning || !mi.SupportsImages || !mi.OnlyReasoning {
		t.Errorf("能力旗标缺失: tool=%v reasoning=%v images=%v onlyReasoning=%v",
			mi.SupportsToolCall, mi.SupportsReasoning, mi.SupportsImages, mi.OnlyReasoning)
	}
	if mi.CanDisableThinking {
		t.Error("CanDisableThinking=true，但客户端配置声明 canDisableThinking=false（思考不可关）")
	}
	if mi.ReasoningSummary != "auto" || mi.Vendor != "j" {
		t.Errorf("summary=%q vendor=%q", mi.ReasoningSummary, mi.Vendor)
	}
	if len(mi.Efforts) != 1 || mi.Efforts[0] != "high" || mi.DefaultEffort != "high" {
		t.Errorf("efforts=%v default=%q want [high]/high", mi.Efforts, mi.DefaultEffort)
	}
	if mi.Description == "" {
		t.Error("Description 为空，客户端选择器会没有描述")
	}
}

// TestExtraModelSeedRealmScoped 种子按 realm 分表：credits 按域定价，
// 跨域复用会写出错误倍率（实测 cn:hy4-preview=x0.29 vs global:hy4-preview=x0.00）。
func TestExtraModelSeedRealmScoped(t *testing.T) {
	if _, ok := ExtraModelSeed("cn", "hy4-preview-f"); ok {
		t.Error("cn 域不应命中 hy4-preview-f 种子（该值取自 global 客户端配置，跨域复用会错）")
	}
	if _, ok := ExtraModelSeed("global", "hy4-preview-f"); !ok {
		t.Error("global 域应命中")
	}
}

// TestExtraModelSeedMissAndEmpty 未收录 id / 空 id 一律 miss，
// 调用方据此退化为裸 ID 条目（不编造字段）。
func TestExtraModelSeedMissAndEmpty(t *testing.T) {
	for _, tc := range []struct{ realm, id string }{
		{"global", "no-such-model-xyz"},
		{"global", ""},
		{"", ""},
		{"bogus-realm", "hy4-preview-f"},
	} {
		if _, ok := ExtraModelSeed(tc.realm, tc.id); ok {
			t.Errorf("realm=%q id=%q 不应命中", tc.realm, tc.id)
		}
	}
}

// TestExtraModelSeedEmptyRealmIsCN 空 realm 归一为 cn（与 realmKey 同口径）。
func TestExtraModelSeedEmptyRealmIsCN(t *testing.T) {
	if _, ok := ExtraModelSeed("", "hy4-preview-f"); ok {
		t.Error("空 realm 应视为 cn ⇒ 不命中 global 种子")
	}
	if _, ok := ExtraModelSeed("cn", "hy4-preview-f"); ok {
		t.Error("cn 不命中")
	}
}
