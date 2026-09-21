package pool

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 探针结论（CoolProbe）的核心契约 —— 用户 2026-09-21：
// 「探测记录值保存 12 小时，之后默认恢复可以被切换器选择」。
//
// 这里测的就是这句话的两个方向：
//   - 有效期内：账号确实被切换器排除（否则「额度不够的不再接入」没落地）；
//   - 到期后：账号**自动**回到默认候选集，不需要任何显式复位动作
//     （否则一次误判就把好号永久钉死）。
func TestCooldownProbeExpiresBackToRotation(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	p.CooldownProbe("u1", "14018 probe credit exhausted")

	// 有效期内：不可选（normal 与全冷却兜底都不给）。
	if got := p.Pick(); got != nil {
		t.Fatalf("探针结论有效期内不应被选中，实际 %+v", got)
	}
	reason, until, ok := p.ProbeVerdictOf("u1")
	if !ok {
		t.Fatal("刚写入的探针结论应为有效")
	}
	if reason != "14018 probe credit exhausted" {
		t.Errorf("reason=%q", reason)
	}
	// TTL 必须是 12h（允许秒级调度抖动）。
	if d := time.Until(until); d > ProbeVerdictTTL || d < ProbeVerdictTTL-time.Minute {
		t.Errorf("有效期 %v，want 约 %v", d, ProbeVerdictTTL)
	}
	if st, _ := p.Status("u1"); !st.Cooling || st.CoolKind != CoolProbe.String() {
		t.Errorf("status 应显示探针冷却：%+v", st)
	}

	// 到期（把 until 拨到过去模拟 12h 后）：无需任何显式复位，账号自动回轮换。
	p.mu.Lock()
	p.byUID["u1"].until = time.Now().Add(-time.Second)
	p.mu.Unlock()

	if _, _, ok := p.ProbeVerdictOf("u1"); ok {
		t.Error("过期的探针结论不应再报告为有效")
	}
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("12h 到期后账号应回到默认候选集，实际 %+v", got)
	}
}

// 全冷却兜底不得把探针判定的额度耗尽号捞回来：它的失败形态是"调了必 14018"，
// 与 CoolHard 同档（见 pickEarliestExpiryLocked）。
func TestCooldownProbeExcludedFromFallback(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownProbe("u1", "14018 probe credit exhausted")

	if got := p.Pick(); got != nil {
		t.Fatalf("探针额度耗尽号不应参与兜底，实际 %+v", got)
	}
}

// 余额刷新回正（scheduler 每 5 分钟一轮的 ReenableIfCredits）应清掉探针结论 ——
// 这是「充值后自动回池」的既有语义，探针结论不该把它挡住。
func TestCooldownProbeClearedByCreditsRecovery(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownProbe("u1", "14018 probe credit exhausted")

	p.ReenableIfCredits("u1", 500, 1000)

	if _, _, ok := p.ProbeVerdictOf("u1"); ok {
		t.Error("余额恢复后探针结论应被清掉")
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("余额恢复后账号应回轮换，实际 %+v", got)
	}
}

// ProbeCooldownClear 只撤探针自己写的结论：真实流量写的 CoolHard/CoolSoft 不归探针管，
// 否则探针会把正在限流的号放回轮换（"面板显示可用、真实请求连续失败"）。
func TestProbeCooldownClearOnlyTouchesProbe(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})

	p.Cooldown("u1", CoolHard, time.Hour, "真实流量 14018")
	if p.ProbeCooldownClear("u1") {
		t.Error("CoolHard 不是探针写的，不应被 ProbeCooldownClear 撤销")
	}
	if st, _ := p.Status("u1"); !st.Cooling {
		t.Errorf("CoolHard 冷却被误清：%+v", st)
	}

	p.CooldownProbe("u2", "14018 probe credit exhausted")
	if !p.ProbeCooldownClear("u2") {
		t.Error("探针写的结论应可被撤销")
	}
	if _, _, ok := p.ProbeVerdictOf("u2"); ok {
		t.Error("撤销后不应再报告有效结论")
	}
	// 重复撤销：已无结论 ⇒ 返回 false（调用方不该报"已解除"）。
	if p.ProbeCooldownClear("u2") {
		t.Error("第二次撤销应返回 false")
	}
}

// 探针结论要跨重启存活：12h 的有效期不该被一次容器重启抹掉（否则频繁重启 = 探针白跑）。
func TestCooldownProbePersists(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownProbe("u1", "14018 probe credit exhausted")
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	reason, _, ok := p2.ProbeVerdictOf("u1")
	if !ok || reason != "14018 probe credit exhausted" {
		t.Fatalf("探针结论未跨重启保留：ok=%v reason=%q", ok, reason)
	}
	if got := p2.Pick(); got != nil {
		t.Fatalf("重启后仍应在冷却中，实际 %+v", got)
	}
}
