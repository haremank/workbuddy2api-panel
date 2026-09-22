package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// enableGlobalRealm 固定 globalEnabled=true（realm 判定依赖该包级开关；本文件所有
// 用例都按 cn/global 双域构造），并在用例结束后还原，避免污染其它测试。
func enableGlobalRealm(t *testing.T) {
	t.Helper()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
}

// TestRetryAfterHintEarliestAcrossRealm：429 的 Retry-After 兜底必须取**该 realm 内
// 最早恢复**的账号。两个号都在冷却（1h / 30s）→ 期望拿到 30s 那个。
// 若实现误取"map 遍历先撞上的那个"（遍历无序 ⇒ 随机 flake）或"最晚的"，客户端会被迫
// 多等 30 分钟 —— 退避给长了比不给更糟：客户端会在本可重试时继续放弃。
func TestRetryAfterHintEarliestAcrossRealm(t *testing.T) {
	enableGlobalRealm(t)

	p := New("")
	p.Add(&auth.Auth{UID: "slow", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "fast", Domain: "www.workbuddy.ai"})
	p.SetCredits("slow", 100, 0)
	p.SetCredits("fast", 100, 0)
	p.Cooldown("slow", CoolSoft, time.Hour, "429 rate limit")
	p.Cooldown("fast", CoolSoft, 30*time.Second, "429 rate limit")

	d, ok := p.RetryAfterHint("global", "m")
	if !ok {
		t.Fatal("池内两号都在冷却，应给出兜底等待时长")
	}
	if d <= 0 || d > 30*time.Second {
		t.Fatalf("应取最早到期者（~30s），got %v", d)
	}
}

// TestRetryAfterHintModelLevelCoolingCounts：**模型级**冷却必须计入（此时账号级完全健康）。
// 这是 round9 那个「模型级冷却覆盖全 realm ⇒ 即时 503」缺陷的同构场景：只读 expiry() 的
// 实现（不含 modelCooldowns）在这里会返回 ok=false ⇒ 客户端拿不到任何退避依据，
// 而 14003 正是本网关最常撞的 429 形态。
func TestRetryAfterHintModelLevelCoolingCounts(t *testing.T) {
	enableGlobalRealm(t)

	p := New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p.SetCredits("g1", 100, 0)
	p.CooldownModelRateLimit("g1", 45*time.Second, "m-cooled", "14003 model rate limit")

	// 前提校验：账号级健康（不指定模型时可选）⇒ 下面 ok=true 只可能来自模型级冷却。
	if got := p.PickExcludingForRealm(nil, "", "global"); got == nil {
		t.Fatal("账号级健康：不指定模型时应可选（说明本用例构造有误）")
	}

	d, ok := p.RetryAfterHint("global", "m-cooled")
	if !ok {
		t.Fatal("模型级冷却（账号级健康）必须计入兜底：只读 expiry() 的实现会漏掉它")
	}
	if d <= 0 || d > 45*time.Second {
		t.Fatalf("应取模型级冷却到期时刻（~45s），got %v", d)
	}
	// 换一个模型：该号健康 ⇒ 无冷却可折算 ⇒ 不发头（否则会误导客户端白等）。
	if d, ok := p.RetryAfterHint("global", "other-model"); ok {
		t.Errorf("其它模型上该号健康，不该给出等待时长：got %v", d)
	}
}

// TestRetryAfterHintOmitsWhenNothingToWaitFor：不该把"等外部动作"折算成秒数。
// 覆盖四条排除线：池内全健康（无可等）、禁用（人工重登）、余额耗尽（充值/签到）、
// CoolHard（次日 04:00）、CoolProbe（12h 探针结论）。这些等的是人的动作或跨天事件，
// 报一个 43xx 秒只会让客户端在错误的时间点重试。
func TestRetryAfterHintOmitsWhenNothingToWaitFor(t *testing.T) {
	enableGlobalRealm(t)

	// ① 池内全部健康：没有"等多久"可言。
	p := New("")
	p.Add(&auth.Auth{UID: "ok", Domain: "www.workbuddy.ai"})
	p.SetCredits("ok", 100, 0)
	if d, ok := p.RetryAfterHint("global", "m"); ok {
		t.Errorf("池内无冷却账号时不该给出等待时长：got %v", d)
	}

	// ② 只有"等外部动作"的号。
	p2 := New("")
	for _, uid := range []string{"disabled", "zero", "hard", "probe"} {
		p2.Add(&auth.Auth{UID: uid, Domain: "www.workbuddy.ai"})
	}
	p2.SetCredits("disabled", 100, 0)
	p2.Disable("disabled", "account banned by upstream")
	p2.SetCredits("zero", 0, 0) // creditsKnown=true 且为 0 ⇒ 上游必回 14018
	p2.SetCredits("hard", 100, 0)
	p2.Cooldown("hard", CoolHard, 4*time.Hour, "14018 credits exhausted")
	p2.SetCredits("probe", 100, 0)
	if !p2.CooldownProbe("probe", "probe verdict: 14018 credits exhausted") {
		t.Fatal("前提失败：CooldownProbe 未生效")
	}

	if d, ok := p2.RetryAfterHint("global", "m"); ok {
		t.Errorf("全部候选都在等外部动作（充值/签到/人工登录），不该给出秒数：got %v", d)
	}
}

// TestRetryAfterHintRealmFiltered：跨域不串。cn 的冷却不该被 global 请求读到
// ——否则客户端会按别域的时间退避（且把别域状态泄漏给客户端）。
func TestRetryAfterHintRealmFiltered(t *testing.T) {
	enableGlobalRealm(t)

	p := New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p.SetCredits("cn1", 100, 0)
	p.SetCredits("g1", 100, 0)
	if st, ok := p.Status("cn1"); !ok || st.Realm != "cn" {
		t.Fatalf("前提失败：cn1 的 realm 应为 cn，got realm=%q ok=%v", st.Realm, ok)
	}
	p.Cooldown("cn1", CoolSoft, time.Hour, "429 rate limit")

	if d, ok := p.RetryAfterHint("global", "m"); ok {
		t.Errorf("global 域请求不该读到 cn 域账号的冷却：got %v", d)
	}
	d, ok := p.RetryAfterHint("cn", "m")
	if !ok || d <= 0 || d > time.Hour {
		t.Errorf("cn 域应读到 cn 账号的冷却（~1h）：got %v ok=%v", d, ok)
	}
}
