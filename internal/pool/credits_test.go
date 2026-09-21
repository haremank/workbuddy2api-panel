package pool

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 余额判据（entry.creditsKnown + credits<=0 ⇒ 不可选）的回归测试。
//
// 背景（2026-09-21 实测诊断）：上游对 0 余额账号一律返回 HTTP 429 + code 14018
// （Credits exhausted），连限免模型也不放行。修复前 healthy() 完全不看 credits、
// 余额刷新在 remain==0 时只赋值不冷却 ⇒ 0 额度号一直留在池里被反复选中且每次必失败。

// TestPickSkipsKnownZeroCredits 已确认耗尽的号必须退出轮换。
func TestPickSkipsKnownZeroCredits(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "rich"})
	p.Add(&auth.Auth{UID: "broke"})
	p.SetCredits("rich", 300, 380)
	p.SetCredits("broke", 0, 390) // 成功查到余额 = 确实耗尽
	for i := 0; i < 200; i++ {
		got := p.Pick()
		if got == nil {
			t.Fatalf("iter %d: pick=nil, rich 应始终可选", i)
		}
		if got.UID == "broke" {
			t.Fatalf("iter %d: 选中了已耗尽的号（credits=0 且 creditsKnown）", i)
		}
	}
}

// TestPickAllowsUnknownCredits 余额**从未查到**（creditsKnown=false）时，credits 的 0
// 是"未知"而非"耗尽"，账号仍必须可选——否则余额查询故障会把整池误判为耗尽。
func TestPickAllowsUnknownCredits(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "fresh1"})
	p.Add(&auth.Auth{UID: "fresh2"})
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		got := p.Pick()
		if got == nil {
			t.Fatal("未查询过余额的号不得被判为耗尽，pick 不应为 nil")
		}
		seen[got.UID] = true
	}
	if len(seen) != 2 {
		t.Fatalf("两个未知余额的号都应参与选号，seen=%v", seen)
	}
}

// TestPickExcludingForModelSkipsKnownZeroCredits healthyForModel 委托 healthy，
// 模型感知选号同样必须排除已耗尽的号。
func TestPickExcludingForModelSkipsKnownZeroCredits(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "ok"})
	p.Add(&auth.Auth{UID: "broke"})
	p.SetCredits("ok", 100, 380)
	p.SetCredits("broke", 0, 390)
	for i := 0; i < 100; i++ {
		got := p.PickExcludingForModel(nil, "deepseek-v4.1-flash")
		if got == nil || got.UID != "ok" {
			t.Fatalf("iter %d: pick=%+v want ok", i, got)
		}
	}
}

// TestReenableIfCreditsZeroDropsFromRotation 余额刷新查到 remain==0 时，账号必须立即
// 退出轮换（这是"5 分钟一次的余额刷新把耗尽号摘出去"的落点）。
func TestReenableIfCreditsZeroDropsFromRotation(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.SetCredits("a", 100, 380)
	p.SetCredits("b", 50, 390)
	// 刷新前 b 可选
	if !p.ServableNow() {
		t.Fatal("servable 应为 true")
	}
	p.ReenableIfCredits("b", 0, 390) // 刷新发现耗尽
	for i := 0; i < 100; i++ {
		if got := p.Pick(); got == nil || got.UID != "a" {
			t.Fatalf("iter %d: pick=%+v，b 已耗尽不该再被选中", i, got)
		}
	}
	st, _ := p.Status("b")
	if !st.CreditsKnown || st.Credits != 0 {
		t.Fatalf("status=%+v want credits=0 credits_known=true", st)
	}
}

// TestReenableIfCreditsRecoveryReturnsToPool 余额恢复后自动回池（无需人工干预）。
func TestReenableIfCreditsRecoveryReturnsToPool(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "broke"})
	p.SetCredits("broke", 0, 390)
	if p.Pick() != nil {
		t.Fatal("耗尽号不该被选中")
	}
	p.ReenableIfCredits("broke", 500, 890) // 充值/签到恢复
	got := p.Pick()
	if got == nil || got.UID != "broke" {
		t.Fatalf("余额恢复后应回池，pick=%+v", got)
	}
}

// TestPickFallbackSkipsKnownZeroCredits 全冷却兜底也不得选中已耗尽的号
// （调了必 14018，纯属浪费轮换）。
func TestPickFallbackSkipsKnownZeroCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "cooling_ok"})
	p.Add(&auth.Auth{UID: "cooling_broke"})
	p.SetCredits("cooling_ok", 100, 380)
	p.SetCredits("cooling_broke", 0, 390)
	// 两个号都进冷却 → 主路径无 healthy 候选 → 走 pickEarliestExpiryLocked 兜底。
	p.Cooldown("cooling_ok", CoolSoft, time.Hour, "test")
	p.Cooldown("cooling_broke", CoolSoft, time.Hour, "test")
	for i := 0; i < 20; i++ {
		got := p.Pick()
		if got == nil || got.UID != "cooling_ok" {
			t.Fatalf("iter %d: 兜底选中了耗尽号 %+v", i, got)
		}
	}
}

// TestPickFallbackNilWhenOnlyKnownZeroCredits 池里只剩耗尽号时兜底返回 nil
// （宁可 503 也不去打必然失败的请求）。
func TestPickFallbackNilWhenOnlyKnownZeroCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "only_broke"})
	p.SetCredits("only_broke", 0, 390)
	if got := p.Pick(); got != nil {
		t.Fatalf("池内只有耗尽号时 pick 应为 nil，got=%+v", got)
	}
}

// TestCountsDetailedExcludesKnownZeroCredits healthy 计数必须把耗尽号算作不可服务，
// 否则 /healthz 会报出 "healthy:7/7" 这种掩盖真实不可用的读数。
func TestCountsDetailedExcludesKnownZeroCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.SetCredits("a", 100, 380)
	p.SetCredits("b", 0, 390)
	total, healthy, cooling, disabled, _ := p.CountsDetailed()
	if total != 2 {
		t.Fatalf("total=%d want 2", total)
	}
	if healthy != 1 {
		t.Fatalf("healthy=%d want 1（耗尽号不得计入 healthy）", healthy)
	}
	if cooling != 1 {
		t.Fatalf("cooling=%d want 1（耗尽号按不可服务计入 cooling）", cooling)
	}
	if disabled != 0 {
		t.Fatalf("disabled=%d want 0（耗尽≠禁用，不应混为一谈）", disabled)
	}
	if !p.ServableNow() {
		t.Fatal("还有一个可用号，ServableNow 应为 true")
	}
}

// TestServableFalseWhenAllKnownZeroCredits 全池耗尽 ⇒ 不可服务（/healthz 将返回 503）。
func TestServableFalseWhenAllKnownZeroCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.SetCredits("a", 0, 380)
	p.SetCredits("b", 0, 390)
	if p.ServableNow() {
		t.Fatal("全池耗尽时 ServableNow 应为 false")
	}
}

// TestCreditsKnownPersistsAcrossReload 耗尽状态必须跨重启保留，否则重启后 0 额度号
// 会重新混入轮换。
func TestCreditsKnownPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "broke"})
	p.SetCredits("broke", 0, 390)
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "broke"})
	if got := p2.Pick(); got != nil {
		t.Fatalf("重启后耗尽号仍被选中：%+v", got)
	}
	st, ok := p2.Status("broke")
	if !ok || !st.CreditsKnown || st.Credits != 0 {
		t.Fatalf("重启后 creditsKnown 丢失：%+v ok=%v", st, ok)
	}
}

// TestCreditsUnknownRestoredFromLegacyState 旧 state 文件（无 credits_known 字段）且
// CreditsTotal==0 时视为"未知"，不得据 credits==0 把号判死——向后兼容保护。
func TestCreditsUnknownRestoredFromLegacyState(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "legacy"})
	// 直接构造旧格式：credits=0 且 credits_total 缺省（=0），credits_known 缺省（=false）。
	p.mu.Lock()
	e := p.byUID["legacy"]
	e.credits = 0
	e.creditsTotal = 0
	e.creditsKnown = false
	p.mu.Unlock()
	p.dirty.Store(true) // Flush 幂等（无变更不写盘）：直接改字段必须自己置脏
	p.Flush()
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("state 未落盘：%v", err)
	}
	// 旧格式特征：credits_total / credits_known 都因零值被 omitempty 省略。
	if bytes.Contains(raw, []byte(`"credits_total"`)) || bytes.Contains(raw, []byte(`"credits_known"`)) {
		t.Fatalf("state 不该含 credits_total/credits_known：%s", raw)
	}

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "legacy"})
	got := p2.Pick()
	if got == nil || got.UID != "legacy" {
		t.Fatalf("旧 state（余额未知）不应把号判死，pick=%+v", got)
	}
	st, _ := p2.Status("legacy")
	if st.CreditsKnown {
		t.Fatalf("旧 state 恢复后 creditsKnown 应为 false，got %+v", st)
	}
}

// TestCreditsKnownRestoredFromLegacyStateWithTotal 旧 state 文件虽无 credits_known，
// 但 CreditsTotal>0 说明查到过余额 ⇒ 兜底推断为已知（0 即耗尽）。
func TestCreditsKnownRestoredFromLegacyStateWithTotal(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "legacy"})
	p.mu.Lock()
	e := p.byUID["legacy"]
	e.credits = 0
	e.creditsTotal = 390
	e.creditsKnown = false // 模拟旧文件没有该字段
	p.mu.Unlock()
	p.dirty.Store(true) // Flush 幂等（无变更不写盘）：直接改字段必须自己置脏
	p.Flush()
	// 先证伪"根本没写盘"：文件必须存在且含预期的 credits_total。
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("state 未落盘：%v", err)
	}
	if !bytes.Contains(raw, []byte(`"credits_total": 390`)) {
		t.Fatalf("state 内容不符预期：%s", raw)
	}

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "legacy"})
	if got := p2.Pick(); got != nil {
		t.Fatalf("CreditsTotal>0 应兜底推断为已知耗尽，不该被选中：%+v", got)
	}
}

// TestStatusServableAndCreditsUpdated Status 必须透出「此刻是否可选」与「余额读数时刻」：
// 前者让面板额度水位视图区分「账面有余额」与「真选得中」，后者是它的鲜度来源
// （后台余额任务每 balance_refresh_minutes 刷新，读数过旧即不可信）。
func TestStatusServableAndCreditsUpdated(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "rich"})
	p.Add(&auth.Auth{UID: "broke"})
	p.Add(&auth.Auth{UID: "fresh"})
	before := time.Now()
	p.SetCredits("rich", 300, 380)
	p.SetCredits("broke", 0, 390)
	after := time.Now()

	st, ok := p.Status("rich")
	if !ok {
		t.Fatal("rich 缺失")
	}
	if !st.Servable {
		t.Error("余额充足的号应 servable")
	}
	if st.CreditsUpdated.Before(before) || st.CreditsUpdated.After(after) {
		t.Errorf("credits_updated=%v 不在 [%v, %v] 内", st.CreditsUpdated, before, after)
	}

	st, _ = p.Status("broke")
	if st.Servable {
		t.Error("已确认耗尽的号不应 servable")
	}
	if !st.CreditsKnown {
		t.Error("成功查询写入的余额应置 credits_known=true")
	}

	st, _ = p.Status("fresh")
	if !st.Servable {
		t.Error("余额未知的号仍应 servable（防余额查询故障误伤整池）")
	}
	if !st.CreditsUpdated.IsZero() {
		t.Errorf("从未成功查询过余额时 credits_updated 应为零值，got %v", st.CreditsUpdated)
	}
}

// TestCreditsUpdatedPersists 读数时刻跨重启保留，否则重启后面板会把旧读数当"刚刚刷新"。
func TestCreditsUpdatedPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100, 380)
	p.Flush()
	want, ok := p.Status("u1")
	if !ok || want.CreditsUpdated.IsZero() {
		t.Fatalf("写盘前 credits_updated 缺失：%+v ok=%v", want, ok)
	}

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	got, ok := p2.Status("u1")
	if !ok {
		t.Fatal("重启后 u1 缺失")
	}
	if !got.CreditsUpdated.Equal(want.CreditsUpdated) {
		t.Fatalf("credits_updated 未跨重启保留：got=%v want=%v", got.CreditsUpdated, want.CreditsUpdated)
	}
	if !got.CreditsKnown {
		t.Error("credits_known 也应随文件恢复")
	}
}

// TestServableIncludesCooling 冷却中的号 servable 必须为 false（面板据此标"冷却中"）。
func TestServableIncludesCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 300, 380)
	if st, _ := p.Status("u1"); !st.Servable {
		t.Fatal("冷却前应 servable")
	}
	p.Cooldown("u1", CoolSoft, time.Hour, "test")
	st, _ := p.Status("u1")
	if st.Servable {
		t.Error("冷却中不应 servable")
	}
	if !st.Cooling {
		t.Error("cooling 应为 true")
	}
}
