// 冷却与熔断：Cooldown（固定时长账号级冷却）、CooldownSoftRate（账号级软冷却，对齐
// 上游重置时间或有界退避）、CooldownSoftForModel（模型级软冷却，对齐重置墙钟）、
// CooldownModelRateLimit（模型级软冷却，无重置时刻时的有界退避）、
// BlockModelBackoff/Clear（11102 负缓存）、软冷却封顶、熔断失败累计、签到解冻。
package pool

import (
	"strings"
	"time"
)

// SetCredits 更新账号余额。同时置 creditsKnown：本方法只在余额查询**成功**时被调用，
// 因此它写入的 credits 是权威值——0 表示"确实耗尽"（选号侧据此出池），而非"未知"。
func (p *Pool) SetCredits(uid string, credits, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = credits
		e.creditsTotal = total
		e.creditsKnown = true
		e.creditsUpdated = time.Now()
		p.dirty.Store(true)
	}
}

// SetCreditsDetailed 更新账号余额/总额 + 快过架子集（签到与余额刷新时调用，
// 供选号优先消耗快过期积分）。expiring 会被钳到 [0, credits]：上游分桶异常时
// 不污染权重。
func (p *Pool) SetCreditsDetailed(uid string, credits, total, expiring int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		if expiring < 0 {
			expiring = 0
		}
		if expiring > credits {
			expiring = credits
		}
		e.credits = credits
		e.creditsTotal = total
		e.creditsExpiring = expiring
		e.creditsKnown = true // 同 SetCredits：成功查询写入的权威值
		e.creditsUpdated = time.Now()
		p.dirty.Store(true)
	}
}

// Cooldown 冷却账号至 now+d（即时冷却：CoolHard 余额耗尽 / CoolSoft 固定短冷却）。
//
// 重构后本入口是「固定时长的账号级冷却」，不再做两件旧事：
//   - 不再喂熔断器失败计数：熔断器只对「反复失败」（NoteError，5xx）退避。
//     软限流/余额耗尽各有权威恢复时刻（重置墙钟 / 04:00 签到），再并入"连续失败"
//     会让用户正常重试越堆越厚。熔断语义由 NoteError 唯一驱动（与 until 正交保持）。
//   - 不再做 softStreak 指数堆加：固定 d 即最终时长。CoolSoft 的精确对齐请用
//     CooldownSoftRate（有界、对齐上游重置时间）。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(d)
		e.coolKind = kind
		e.reason = reason
		// 非模型级冷却入口：清空模型级独立冷却表（modelCooldowns），
		// 避免上一次模型级限流的模型豁免泄漏到本次**账号级**限流上
		// （否则换模型请求会错误绕过本次冷却）。
		e.modelCooldowns = nil
		p.dirty.Store(true)
	}
}

// CooldownSoftForModel 429 的**模型级**软冷却入口（issue #31）：把该模型的冷却截止
// 精确对齐到上游重置墙钟（不做指数堆加、不做 softStreak 计数）。
//
//   - resetAt 非零（带解析时间）→ modelCooldowns[model].Until = min(resetAt,
//     now+softRateMax)，ResetAt 记录上游原始墙钟（台账 ResetAt）。不写 until
//     （全账号级冷却不受模型级限流污染），切模型即可用（模型豁免）。
//   - resetAt 零值（无时间文案）→ 有界退避：base 起按 softStreak 翻倍、封顶
//     softRateMax，且**在软冷却中**（until 未到期）时不推进/不延长（兜底探测不再把
//     冷却越堆越厚）。不记录模型（不豁免）。
//
// 与旧实现的差异：有上游重置时间时绝对不做指数堆加；无重置时间时，「冷却中兜底
// 探测再 429」不再 softStreak++ 翻倍——这正是用户「全池被推到 2h 封顶」的元凶。
func (p *Pool) CooldownSoftForModel(uid string, base time.Duration, resetAt time.Time, model, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		now := time.Now()
		if !resetAt.IsZero() {
			// 有上游重置时间：冷却截止 = min(resetAt, now+softRateMax)，不做指数放大。
			if e.modelCooldowns == nil {
				e.modelCooldowns = map[string]modelCooldown{}
			}
			e.modelCooldowns[model] = modelCooldown{
				Until:   p.cappedSoftUntilLocked(now, resetAt),
				ResetAt: resetAt,
				Reason:  reason,
			}
		} else {
			// 无解析时间（普通软冷却）：有界退避（base 起按 softStreak 翻倍、封顶
			// softRateMax）。注意：**在软冷却中**（until 未到期）时不推进/不延长。
			if e.coolKind != CoolSoft || !now.Before(e.until) {
				d := p.softDurationLocked(base, e.softStreak+1)
				e.softStreak++
				e.until = now.Add(d)
			}
			e.coolKind = CoolSoft
			e.reason = reason
			e.modelCooldowns = nil
		}
		p.dirty.Store(true)
	}
}

// CooldownModelRateLimit 模型级限流的**模型级有界退避**入口（上游不给重置时刻时用）。
//
// 与 CooldownSoftForModel 的分工（两者都只冻模型、不冻整号）：
//   - CooldownSoftForModel：上游**明说**重置墙钟（6004「将在 … 重置」）→ 对齐该墙钟。
//   - 本方法：上游只说 "too many requests"、**不给**恢复时刻（global realm 的 14003）
//     → 冷却 = now + min(base, soft_rate_max)，有界、不指数堆加。
//
// 为什么必须模型级：2026-09-21 实测同一账号同一时刻
// `hy4-preview-f ✅200 | hy4-preview 429/14003 | hy3 ✅200 | deepseek-v4.1-flash ✅200`
// —— 被限的只是该模型。归账号级会把整号冻结 soft_rate（默认 60s），同号其它健康模型
// 一起连坐，正是「接入国际版模型老是没反应」的第二根因。
//
// 细节：
//   - 不缩短已生效的冷却（取较晚的 Until）；既有 11102 负缓存的 reason 前缀保留
//     （BlockModelClear 靠 "11102" 前缀识别，不能被 14003 覆盖掉）。
//   - Hits 原样保留：14003 无权威恢复时刻，不参与指数堆加（越堆越厚是旧实现的老毛病）。
//   - ResetAt 保持零值：上游没给时刻，绝不编造。
func (p *Pool) CooldownModelRateLimit(uid string, base time.Duration, model, reason string) {
	if uid == "" || model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	now := time.Now()
	until := now.Add(base)
	if cap := p.softRateMaxOr(); until.After(now.Add(cap)) {
		until = now.Add(cap)
	}
	if e.modelCooldowns == nil {
		e.modelCooldowns = map[string]modelCooldown{}
	}
	prev := e.modelCooldowns[model]
	if prev.Until.After(until) {
		until = prev.Until
	}
	keepReason := reason
	if strings.HasPrefix(prev.Reason, "11102") {
		keepReason = prev.Reason
	}
	e.modelCooldowns[model] = modelCooldown{
		Until:  until,
		Reason: keepReason,
		Hits:   prev.Hits,
	}
	p.dirty.Store(true)
}

// modelBlock TTL 常量（11102 负缓存退避）：
// 首次命中 6h；半开到期后允许放行重试，再次命中 TTL = base × 2^min(hits-1, shift)；
// 封顶 24h（最多一天再试一次）。该模型请求成功即由 BlockModelClear 清除。
const (
	modelBlockBaseTTL = 6 * time.Hour
	modelBlockShift   = 4
	modelBlockMaxTTL  = 24 * time.Hour
)

// BlockModelBackoff 11102「该后端无此模型」的 (账号, 模型) 负缓存入口
// （handler.applyErrorPolicy 调用）。复用 modelCooldowns 机制（不新建平行状态）：
// 写 modelCooldowns[model]，Until 为指数退避 TTL，选号侧 healthyForModel 自动对该
// 账号避开该模型。
//
// 语义与 6004 正交：6004 是「模型被限流、对齐重置墙钟」，本入口是「官方确定该后端
// 无此模型、重试无意义，只能换模型/换账号」。resetAt 无需传（11102 无重置文案），
// ResetAt 保持零值，与 6004 台账共用 Until 判定——11102 条目会以 11102 reason 出现在
// /status 台账，运维可见。
func (p *Pool) BlockModelBackoff(uid, model, reason string) {
	if uid == "" || model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	now := time.Now()
	hits := 0
	if e.modelCooldowns != nil {
		hits = e.modelCooldowns[model].Hits
	}
	hits++
	ttl := modelBlockBaseTTL
	if d := ttl * (1 << uint(min(hits-1, modelBlockShift))); d < modelBlockMaxTTL {
		ttl = d
	} else {
		ttl = modelBlockMaxTTL
	}
	if e.modelCooldowns == nil {
		e.modelCooldowns = map[string]modelCooldown{}
	}
	e.modelCooldowns[model] = modelCooldown{
		Until:  now.Add(ttl),
		Reason: reason,
		Hits:   hits,
	}
	p.dirty.Store(true)
}

// BlockModelClear 清除 (账号, 模型) 的 11102 负缓存条目（该模型实测又通了）。半开探测
// 或正常请求对该模型成功后调用（handler 成功路径）。只清 11102 条目、不碰 6004 独立
// 冷却表——6004 有自身上游重置墙钟语义，成功不该抹掉。reason 前缀判定区分两者：
// 11102 条目的 reason 恒以 "11102" 开头（见 upstream.BlockModelReason）。
func (p *Pool) BlockModelClear(uid, model string) {
	if uid == "" || model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || len(e.modelCooldowns) == 0 {
		return
	}
	mc, exists := e.modelCooldowns[model]
	if !exists || !strings.HasPrefix(mc.Reason, "11102") {
		return
	}
	delete(e.modelCooldowns, model)
	if len(e.modelCooldowns) == 0 {
		e.modelCooldowns = nil
	}
	p.dirty.Store(true)
}

// ModelCooldownClear 清除 (账号, 模型) 的**任意**模型级冷却条目，返回是否真的清掉了。
//
// 与 BlockModelClear 的分工（两者不可互相替代）：
//   - BlockModelClear 只清 reason 以 "11102" 开头的负缓存 —— 它服务的是「正常请求对该模型
//     成功」这条路径，此时 6004 的上游重置墙钟语义不该被抹掉。
//   - 本方法是**实测恢复**入口：探针真实打了一发拿到 200，这比任何墙钟/退避都权威，
//     因此 14003（模型级限流）与 6004（对齐上游墙钟）的条目都应解除 —— 否则面板上
//     「探针显示可用、选号器却仍避开」会长期自相矛盾。
//
// 只影响该 uid 的该 model，不动账号级冷却（until/coolKind）与其它模型条目。
func (p *Pool) ModelCooldownClear(uid, model string) bool {
	if uid == "" || model == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || len(e.modelCooldowns) == 0 {
		return false
	}
	if _, exists := e.modelCooldowns[model]; !exists {
		return false
	}
	delete(e.modelCooldowns, model)
	if len(e.modelCooldowns) == 0 {
		e.modelCooldowns = nil
	}
	p.dirty.Store(true)
	return true
}

// ProbeVerdictTTL 探针实测结论的有效期。用户 2026-09-21 指定 12 小时：
// 「探测记录值保存 12 小时，之后默认恢复可以被切换器选择」。
//
// 为什么必须有 TTL（而不是让探针结论一直生效）：探针是**一次快照**，不是持续观测。
// 上游可能已经恢复（充值到账、模型级限流窗口过去、上游抖动结束），但探针不会自己知道；
// 若无 TTL，一次误判会把好号永久钉死在池外。12h 到期后账号回到**默认候选集**，
// 由真实流量重新判定 —— 错了只浪费一轮上游调用（现有错误路径会立刻重新出池）。
const ProbeVerdictTTL = 12 * time.Hour

// CooldownProbe 写入一条探针实测结论：账号级出池，有效期固定 ProbeVerdictTTL。
//
// 与 CooldownUntilTomorrow4AM（真实流量撞 14018 的路径）的区别只有时长口径：
// 探针结论是**可撤销的推断**，用固定 12h 而不是跟签到墙钟（global 号不签到，
// 「次日 04:00」对它可能永不带来恢复）。
//
// 到期不需要任何清理动作：until 过了 healthy() 自然放行（与其它冷却同一机制），
// 因此「12h 后默认恢复」是结构性保证，不依赖定时任务。提前恢复的路径有三条，
// 都会清掉本结论：实测 200（ProbeCooldownClear）/ 余额刷新回正（reviveCoolingLocked）/
// 人工解冻（Revive）。
//
// 返回 false = uid 不在池中（无写入）。调用方据此区分"已出池"与"号根本不在池里"，
// 不要对后者报"已出池"。
func (p *Pool) CooldownProbe(uid, reason string) bool {
	if uid == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.until = time.Now().Add(ProbeVerdictTTL)
	e.coolKind = CoolProbe
	e.reason = reason
	// 账号级结论：清空模型级独立冷却表，与 Cooldown 同口径（否则模型豁免会绕过本结论）。
	e.modelCooldowns = nil
	p.dirty.Store(true)
	return true
}

// ProbeVerdictOf 读取该账号当前**仍在有效期内的**探针结论。
// ok=false 表示无结论或已过期（过期即「默认状态」：可被切换器选择）。
// 纯查询，不修改任何状态；供面板/status 展示"这条记录还剩多久"。
func (p *Pool) ProbeVerdictOf(uid string) (reason string, until time.Time, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, found := p.byUID[uid]
	if !found || e.coolKind != CoolProbe {
		return "", time.Time{}, false
	}
	if e.until.IsZero() || !time.Now().Before(e.until) {
		return "", time.Time{}, false // 已过期 ⇒ 等同无结论
	}
	return e.reason, e.until, true
}

// ProbeCooldownClear 清除**仅由探针写入的**账号级结论，返回是否真的清掉了。
// 探针实测拿到 200 时调用：那一发比 12h 前的结论权威，且 14018 是账号级判据
// （额度耗尽对**所有**模型生效）⇒ 任一模型实测通就足以推翻整条结论。
//
// 严格只清 CoolProbe：CoolHard（真实流量撞 14018，等充值/签到）与 CoolSoft
// （真实 429 退避）是真实流量的判据，探针无权替它们恢复 —— 否则探针会把正在
// 限流的号放回轮换，制造"面板显示可用、真实请求连续失败"的假象。
func (p *Pool) ProbeCooldownClear(uid string) bool {
	if uid == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || e.coolKind != CoolProbe {
		return false
	}
	if e.until.IsZero() || !time.Now().Before(e.until) {
		// 已过期：语义上已恢复，清不清都等价；这里仍清掉残留 reason 并如实返回 false
		// （没"撤销"任何还在生效的东西，调用方不该报"已解除"）。
		e.coolKind = 0
		e.reason = ""
		p.dirty.Store(true)
		return false
	}
	e.clearCoolingLocked()
	p.dirty.Store(true)
	return true
}

// ModelCooldowns 返回该账号当前的模型级冷却快照（model → reason），供探针报告写回效果。
// 返回副本，调用方可安全持有。
func (p *Pool) ModelCooldowns(uid string) map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || len(e.modelCooldowns) == 0 {
		return nil
	}
	out := make(map[string]string, len(e.modelCooldowns))
	for m, mc := range e.modelCooldowns {
		out[m] = mc.Reason
	}
	return out
}

// CooldownSoftRate 429/限流文案的**账号级**软冷却入口（handler.applyErrorPolicy 调用）。
//
// 语义：
//   - resetAt 非零（上游带权威重置时间，无论 6004 还是 11140 rate-limiting）→
//     账号级直到该墙钟（截断到 softRateMax，绝不指数堆加）；**不**在
//     modelCooldowns 记模型（账号级语义，不产生切模型豁免——普通账号级限流不该
//     因切模型绕过）。
//   - resetAt 零值且**不在冷却中**（首次/恢复后的新限流）→ 有界退避：按 softStreak
//     指数退避并封顶 softRateMax。softStreak 只在真正进入一次新冷却时计数，由
//     NoteSuccess/reviveCoolingLocked 清零（既有恢复语义）。
//   - resetAt 零值且**已在软冷却中**（兜底探测再次撞 429）→ 不推进 streak、不延长
//     until：用户重试/并发兜底探测不得把冷却越堆越厚——这正是旧实现「越重试越冷、
//     全池被推到 2h 封顶」的元凶（每次探测都 softStreak++ 指数翻倍）。
func (p *Pool) CooldownSoftRate(uid string, base time.Duration, resetAt time.Time, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		now := time.Now()
		if !resetAt.IsZero() {
			e.until = p.cappedSoftUntilLocked(now, resetAt)
		} else if e.coolKind != CoolSoft || !now.Before(e.until) {
			// 新限流（不在有效软冷却中）：推进有界退避；兜底探测（仍在软冷却中）不翻倍。
			d := p.softDurationLocked(base, e.softStreak+1)
			e.softStreak++
			e.until = now.Add(d)
		}
		e.coolKind = CoolSoft
		e.reason = reason
		e.modelCooldowns = nil // 账号级软冷却：清空模型豁免（切模型不绕过）
		p.dirty.Store(true)
	}
}

// cappedSoftUntilLocked 把上游重置墙钟截断到 softRateMax（now+softRateMax 与 resetAt
// 取较早者）。resetAt 已过期（时钟偏移/文案过期）时时长钳到时间零点附近，立即恢复。
// 调用方必须已持有 p.mu。
func (p *Pool) cappedSoftUntilLocked(now, resetAt time.Time) time.Time {
	cap := now.Add(p.softRateMaxOr())
	if resetAt.After(cap) {
		return cap
	}
	if resetAt.After(now) {
		return resetAt
	}
	return now.Add(time.Millisecond)
}

// softRateMaxOr 返回生效的 softRateMax（未注入时按默认 2h），供封顶计算。
// 调用方必须已持有 p.mu。
func (p *Pool) softRateMaxOr() time.Duration {
	if p.softRateMax > 0 {
		return p.softRateMax
	}
	return defaultSoftRateMax
}

// softDurationLocked 按连续软冷却次数把基数 d 指数放大：d << (streak-1)，封顶 softRateMax。
// softRateMax 未注入（<=0）时按 defaultSoftRateMax 算。streak<=1 时原样返回 d。
// 左移位数受 softStreakShiftMax 限制，避免 streak 极大时移位溢出。
// 调用方必须已持有 p.mu。
func (p *Pool) softDurationLocked(d time.Duration, streak int) time.Duration {
	if streak <= 1 {
		return d
	}
	shift := streak - 1
	if shift > softStreakShiftMax {
		shift = softStreakShiftMax
	}
	d <<= shift
	max := p.softRateMax
	if max <= 0 {
		max = defaultSoftRateMax
	}
	if d > max || d <= 0 { // d<=0：左移溢出成负数/零，同样按封顶兜底
		d = max
	}
	return d
}

// recordBreakerFailureLocked 累计一次熔断失败；达到阈值则按指数退避熔断。
// 熔断与冷却（until）解耦：冷却按错误类别给固定时长，熔断则对"反复失败"逐次加长封禁。
// 调用方必须已持有 p.mu。
func (p *Pool) recordBreakerFailureLocked(e *entry) {
	e.fails++
	if e.fails < p.breakerThreshold {
		return
	}
	d := p.breakerCooldown
	for i := 0; i < e.retryCount; i++ {
		d *= 2
		if d >= p.breakerCooldownMax {
			d = p.breakerCooldownMax
			break
		}
	}
	// 触发熔断：重置失败计数供下一轮重新累计；retryCount 递增放大退避指数。
	e.fails = 0
	e.retryCount++
	e.breakerUntil = time.Now().Add(d)
}

// CooldownUntilTomorrow4AM 冷却到下一个 04:00（本地时区）。
// 用于 ErrHardCredit 场景：积分耗尽账号等签到任务（09:00/21:00）恢复。
func (p *Pool) CooldownUntilTomorrow4AM(uid string, reason string) {
	now := time.Now()
	p.Cooldown(uid, CoolHard, nextDay4AM(now).Sub(now), reason)
}

// nextDay4AM 返回 now 之后最近的一个 04:00（与 now 同一时区）。
// now 在当天 04:00 之前（凌晨 00:00~04:00）时返回当天 04:00——此时签到尚未执行，
// 该窗内触发的硬冷却等当天签到即可恢复；返回次日会白冷约一天。
// 04:00 整及之后返回次日 04:00。
// time.Date 对日溢出自动进位（月末→下月 1 号、年末→下年 1 号），天然覆盖跨日/跨月/跨年。
func nextDay4AM(now time.Time) time.Time {
	if now.Hour() < 4 {
		return time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, now.Location())
	}
	return time.Date(now.Year(), now.Month(), now.Day()+1, 4, 0, 0, 0, now.Location())
}

// ReenableIfCredits 签到后解冻：仅当 remain > 0 且账号非禁用时，清冷却（余额恢复）。
// 注意：不碰熔断器——熔断到期（breakerUntil 过期）或下次 chat 成功（NoteSuccess）才恢复。
// reviveCoolingLocked 已迁至 transition.go（状态机迁移唯一权威实现）。
