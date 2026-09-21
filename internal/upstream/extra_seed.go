// extra_seed.go 补充项（config.models.extra_*）的**第三级**元数据兜底。
//
// 查找链（handler.catalogOrSeed）：本 realm 全量 catalog → CN catalog 回落 → 本表。
//
// 为什么需要第三级（2026-09-21 round5 上线后发现）：
// `extra_*` 的语义是「上游能调通、但 agents[cli] 白名单没列出」。绝大多数这类 id
// 仍能在**企业端点全量目录 data.models** 里查到，CatalogLookup 一级就命中。
// 但 `hy4-preview-f` 是反例：它连 data.models 都没有（两级 catalog 全 miss）
// ⇒ /v1/models 退化成裸 ID 条目，只吐 id/object/created/owned_by 四个字段，
// 缺 name / credits / supports_tool_call / max_output_tokens 等 11 个字段。
// 客户端按 /v1/models 渲染模型选择器或按能力旗标决定是否发 tools 时，
// 看到的是一个「没有名字、不支持工具调用、输出上限未知」的残缺项。
//
// 数据来源：**WorkBuddy 官方客户端产品配置** models[] 条目（与促销/计费口径同源，
// 非编造）：
//
//	cn     realm : ~/.workbuddy/cache/acc-product-config-v3.json（copilot.tencent.com）
//	global realm : ~/.workbuddy-ai/cache/acc-product-config-v3.json（www.workbuddy.ai）
//
// 为什么不并进 context_catalog.go：那张表只管 context_length / max_output_tokens
// 两个字段、且 CN/global 共用（上下文是模型固有属性）；本表是全字段元数据，含
// **credits 倍率**——按 realm 定价（实测 cn:hy4-preview=x0.29 而 global:hy4-preview=x0.00）
// ⇒ 必须按 realm 分表，不能跨域复用。
package upstream

// extraModelSeeds 补充项静态元数据（realm → 模型 id → ModelInfo）。
//
// 只收录「两级 catalog 都查不到、但官方客户端配置里有权威值」的模型；
// 能靠 catalog 命中的不要往这里加（会掩盖上游数据源，且值会漂移）。
var extraModelSeeds = map[string]map[string]ModelInfo{
	"global": {
		// hy4-preview-f = 国际版 hy4 的**免费档**（global 客户端配置
		// modelPromotions 里 discount.factor=0 / discountedCredits="0x" 的目标）。
		// 它不在上游企业端点全量目录 data.models 里（两级 catalog 全 miss），
		// 但网关确实能调通（round5 已加入 extra_global，实测 200）。
		//
		// 2026-09-21 取自 global 客户端产品配置 models[]，逐字段对应：
		//   maxInputTokens=1000000  maxOutputTokens=64000  maxAllowedSize=1000000
		//   credits="x0.00"  vendor="j"
		//   supportsImages/supportsReasoning/supportsToolCall=true  onlyReasoning=true
		//   reasoning{defaultEffort:"high", supportedEfforts:["high"], summary:"auto",
		//             canDisableThinking:false}
		//   descriptionZh="混元思考模型，具有增强的推理能力"
		//   name="Hy4 preview"
		// 同族的 hy4-preview 走 catalog 命中（不在此表），其倍率为 x0.29 —— 免费与
		// 收费两档正是靠 -f 后缀区分，故 credits 必须写 x0.00。
		"hy4-preview-f": {
			Name:               "Hy4 preview",
			ContextWindow:      1000000,
			MaxTokens:          64000,
			Efforts:            []string{"high"},
			DefaultEffort:      "high",
			Description:        "混元思考模型，具有增强的推理能力",
			Credits:            "x0.00",
			Vendor:             "j",
			SupportsReasoning:  true,
			SupportsToolCall:   true,
			OnlyReasoning:      true,
			SupportsImages:     true,
			MaxAllowedSize:     1000000,
			CanDisableThinking: false,
			ReasoningSummary:   "auto",
		},
	},
}

// ExtraModelSeed 查补充项的静态元数据兜底（realm → id）。
// realm 空视为 cn（与 realmKey 同口径）；未收录 → ok=false（调用方退化为裸 ID 条目）。
func ExtraModelSeed(realm, id string) (ModelInfo, bool) {
	if id == "" {
		return ModelInfo{}, false
	}
	mi, ok := extraModelSeeds[realmKey(realm)][id]
	if !ok {
		return ModelInfo{}, false
	}
	mi.ID = id // 防御：与 CatalogLookup 同口径，以调用方 id 为准
	return mi, true
}
