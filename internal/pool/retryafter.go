// 429 的 Retry-After 兜底（round14 / N2）：把「池内最早可能重新服务」折算成秒数，
// 供 handler 在 429 响应上写 Retry-After 头。
//
// 背景：上游返回 429 时可能带 Retry-After（本网关此前**只**把它用作冷却时长，
// 从不回客户端）⇒ OpenAI SDK / 各类网关拿不到退避依据，只能按自身经验值立刻重试，
// 反复撞穿限流窗口（放大上游压力，客户端还误以为服务不可用）。兄弟项目
// workbuddy2api-hub 的 429 路径正是「取重置时刻 → 记短冷却 → 换号 continue →
// 回客户端带 Retry-After」（见 RESEARCH-RATELIMIT-20260922.md）。
package pool

import "time"

// rotationExcludedLocked 报告账号是否因「调了必然失败」而被排除在轮换/兜底候选之外：
//   - 跨 realm：本 realm 的请求不该抽到别域的号；
//   - disabled：需人工重新登录，永不自行恢复；
//   - 余额已确认耗尽（creditsKnown && credits<=0）：上游对 0 余额号一律 14018，
//     等的是余额刷新/签到，不是"等几秒"；
//   - CoolHard / CoolProbe 且冷却仍在有效期：依据同为「调了必 14018」，
//     12h（ProbeVerdictTTL）到次日 04:00（签到）量级；
//   - 该模型的 11102 负缓存：上游已确定性答复「无此模型」，试探只会白打一轮。
//
// 为什么单独抽出来：pickEarliestExpiryLocked（全冷却兜底）与 RetryAfterHint（429 的
// Retry-After 兜底）必须**同源**——候选集一旦分歧，客户端就会按一个选号器永远不会去试
// 的时刻退避（或反过来过早重试继续撞限流）。同一事实散落两处必然漂移（本项目既有教训：
// 见 PROBE.md「同一事实散落多份文档必然漂移」）。
//
// 调用方需已持 p.mu（只读遍历）。**不含** inFlightFull：在途占满是秒级租约，不构成
// "何时恢复"的判断依据——兜底侧自己另判（它必须真能立刻租到号）。
func (p *Pool) rotationExcludedLocked(e *entry, now time.Time, realm, reqModel string) bool {
	if realm != "" && e.a.Realm() != realm {
		return true
	}
	if e.disabled {
		return true
	}
	if e.creditsKnown && e.credits <= 0 {
		return true
	}
	if (e.coolKind == CoolHard || e.coolKind == CoolProbe) && !e.until.IsZero() && now.Before(e.until) {
		return true
	}
	return e.modelBackoffCooled(now, reqModel)
}

// RetryAfterHint 估算「该 realm 下、指定模型最早可能重新服务」还需多久，供 handler 在
// 429 响应上写 Retry-After 头（**仅在上游未给该头时**作为兜底；上游给了以上游为准）。
//
// 取各候选账号 expiryForModel(now, reqModel) 的**最早者**。为什么必须用 ForModel 版：
// 模型级限流（6004/14003）下的号账号级完全健康，expiry() 只覆盖账号级三维
// （until/breakerUntil/degradeUntil）⇒ 返回零值 ⇒ 算出的等待时长恒为空。
// 这正是 round9 那个「模型级冷却覆盖全 realm ⇒ 即时 503」缺陷的同一个坑。
//
// ok=false 的三种情形，调用方一律**不发该头**（不编数字）：
//   - 池内无任何冷却中的候选（说明该 429 不是池内状态造成的）；
//   - 候选全部被 rotationExcludedLocked 排除（等的是充值/签到/人工登录，不是秒数）；
//   - 算出的剩余量已 <= 0（读取竞态：刚好到期）。
func (p *Pool) RetryAfterHint(realm, reqModel string) (time.Duration, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	var best time.Time
	for _, e := range p.byUID {
		if p.rotationExcludedLocked(e, now, realm, reqModel) {
			continue
		}
		t := e.expiryForModel(now, reqModel)
		if t.IsZero() {
			continue
		}
		if best.IsZero() || t.Before(best) {
			best = t
		}
	}
	if best.IsZero() {
		return 0, false
	}
	d := best.Sub(now)
	if d <= 0 {
		return 0, false
	}
	return d, true
}
