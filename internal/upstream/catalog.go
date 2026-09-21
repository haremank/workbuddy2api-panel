// catalog.go 上游"全量模型目录"缓存 —— 给 models.extra_* 补充项回填元数据。
//
// 为什么需要它（2026-09-21 独立审核实测）：
//
//	上游企业端点 /console/enterprises/personal/models 的 data.models 有 30 条，
//	但 data.agents[cli].models 白名单只有 16 条。网关的 CN 口径是
//	「/v3/config 全量 ∪ 企业端点 ∩ cli 白名单」，于是被白名单挡掉的模型
//	（deepseek-v3-2-volc / default / hunyuan-2.0-thinking / hunyuan-chat /
//	kimi-k2.5）虽然**上游能调通**，却在 /v1/models 里没有条目、拿不到富字段，
//	只能靠 config.models.extra_cn 补成"裸 ID"。裸 ID 的后果不只是少几个展示字段：
//	context_length 会落到四级查找的 1M 兜底值，而 deepseek-v3-2-volc 上游其实只有
//	96K —— 客户端按 1M 估算就会发出超长 prompt，白丢一次请求。
//
// 本文件把"那一轮已经拉到手、只是被白名单过滤掉的 data.models"留存下来，
// 供补充项按 id 查回权威元数据。**不新增任何上游请求**（数据来自同一次探测）。
//
// 注意 realm 隔离：按 realm 分层桶（与 efforts 的 C-2 原则一致）。
// global 侧的企业端点 data.models 与 cli 白名单等长（18 = 18，实测），
// 没有"被挡掉的"条目，所以 global 补充项在 global 桶里查不到 ——
// 调用方可显式回落查 CN 桶（见 server.applyGlobalModelOverrides 的注释）。
package upstream

// catalogFromDyn 把 dynModelEntry map 转成 ModelInfo 切片（落 catalog 用）。
// map 迭代序随机，但 catalog 以 id 为键、不依赖顺序，无需排序。
func catalogFromDyn(m map[string]dynModelEntry) []ModelInfo {
	out := make([]ModelInfo, 0, len(m))
	for _, e := range m {
		if e.ID == "" {
			continue
		}
		out = append(out, e.modelInfo())
	}
	return out
}

// storeCatalog 按 realm 写入全量目录快照（内部建副本，调用方无需持锁）。
// 空列表不写：避免"某次探测没给全"清掉既有快照。
func (c *Client) storeCatalog(realm string, infos []ModelInfo) {
	if c == nil || len(infos) == 0 {
		return
	}
	m := make(map[string]ModelInfo, len(infos))
	for _, mi := range infos {
		if mi.ID != "" {
			m[mi.ID] = mi
		}
	}
	if len(m) == 0 {
		return
	}
	c.catalogMu.Lock()
	defer c.catalogMu.Unlock()
	if c.catalog == nil {
		c.catalog = make(map[string]map[string]ModelInfo)
	}
	c.catalog[realmKey(realm)] = m
}

// CatalogLookup 按 realm + 模型 id 查全量目录里的元数据（只读）。
// 未探测过该 realm / 目录里没有该 id → (零值, false)。
func (c *Client) CatalogLookup(realm, id string) (ModelInfo, bool) {
	if c == nil || id == "" {
		return ModelInfo{}, false
	}
	c.catalogMu.RLock()
	defer c.catalogMu.RUnlock()
	if c.catalog == nil {
		return ModelInfo{}, false
	}
	mi, ok := c.catalog[realmKey(realm)][id]
	return mi, ok
}

// CatalogOrSeed 补充项元数据的**三级**查找（单一实现，全仓唯一入口）：
//
//  1. 本 realm 全量目录 data.models（CatalogLookup）
//  2. CN 同名条目回落（**仅 global 域**）
//  3. 静态种子表（ExtraModelSeed）
//
// 三级全 miss → (零值, false)，调用方据此退化为裸 ID 条目（不编造字段）。
//
// 为什么抽到 upstream 层（2026-09-21 round6 补）：handler 与 panel 各写过一份
// 等价逻辑，round6 给 handler 加第三级时漏改 panel ⇒ `/v1/models` 与面板「模型与档位」
// 对同一模型给出不同字段（正是本次要修的漂移）。单一实现是唯一防漂移的办法。
//
// 为什么第 2 级只对 global 开：global 后端确实能识别 CN 目录里的同名模型
// （2026-09-21 逐个实测 200，服务账号为 global 号），与 CN 同属一个模型族，
// 能力声明可作客户端展示口径。CN 侧无需回落（本 realm 就是 CN）。
//
// 返回的 ModelInfo.ID 已对齐调用方传入的 id（catalog 以 id 为键，此处防御性重写）。
func (c *Client) CatalogOrSeed(realm, id string) (ModelInfo, bool) {
	if mi, ok := c.CatalogLookup(realm, id); ok {
		mi.ID = id
		return mi, true
	}
	if realmKey(realm) == "global" {
		if mi, ok := c.CatalogLookup("cn", id); ok {
			mi.ID = id
			return mi, true
		}
	}
	return ExtraModelSeed(realm, id)
}

// CatalogSnapshot 返回该 realm 全量目录的副本（面板/调试用；未探测过 → nil）。
func (c *Client) CatalogSnapshot(realm string) map[string]ModelInfo {
	if c == nil {
		return nil
	}
	c.catalogMu.RLock()
	defer c.catalogMu.RUnlock()
	src := c.catalog[realmKey(realm)]
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]ModelInfo, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}
