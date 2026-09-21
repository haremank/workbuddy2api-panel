package pool

import (
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// ---------------------------------------------------------------------------
// 14003（global 模型级限流，无重置文案）→ 模型级有界退避
//
// 2026-09-21 实测：同一账号同一时刻
//   hy4-preview-f ✅200 | hy4-preview 429/14003 | hy3 ✅200 | deepseek-v4.1-flash ✅200
// ⇒ 被限的只是该模型。旧实现把它归账号级，整号冻结 soft_rate（默认 60s），
// 同号其它健康模型一起连坐 —— 「接入国际版模型老是没反应」的第二根因。
// ---------------------------------------------------------------------------

// TestCooldownModelRateLimitModelOnly 只冻该模型、不冻整号。
func TestCooldownModelRateLimitModelOnly(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownModelRateLimit("u1", time.Minute, "hy4-preview", "14003 model rate limit")

	p.mu.RLock()
	e := p.byUID["u1"]
	mc, ok := e.modelCooldowns["hy4-preview"]
	until := e.until
	n := len(e.modelCooldowns)
	p.mu.RUnlock()

	if !ok {
		t.Fatal("应写入 modelCooldowns[hy4-preview]")
	}
	if !until.IsZero() {
		t.Errorf("until=%v 应为零值（模型级限流不得冻整号）", until)
	}
	if n != 1 {
		t.Errorf("modelCooldowns 条目数=%d want 1", n)
	}
	if !mc.ResetAt.IsZero() {
		t.Errorf("ResetAt=%v 应为零值（上游没给恢复时刻，不得编造）", mc.ResetAt)
	}
	if d := time.Until(mc.Until); d < 50*time.Second || d > 70*time.Second {
		t.Errorf("Until 距 now=%v，应 ≈base(60s)", d)
	}

	// 该模型不可选；同号其它模型仍可选；账号本身仍健康。
	now := time.Now()
	if e.healthyForModel(now, "hy4-preview") {
		t.Error("hy4-preview 正处模型级冷却 → healthyForModel 应拦截")
	}
	if !e.healthyForModel(now, "deepseek-v4.1-flash") {
		t.Error("同号 deepseek-v4.1-flash 应仍可选（模型级豁免）")
	}
	if !e.healthy(now) {
		t.Error("账号不应被判定不健康（未写账号级冷却）")
	}
}

// TestCooldownModelRateLimitDoesNotShorten 已生效的更晚冷却不被缩短。
func TestCooldownModelRateLimitDoesNotShorten(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 先来一次带重置墙钟的 6004（30 分钟后恢复）。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(30*time.Minute), "hy4-preview", "6004 model rate limit")
	// 再来一次无时刻的 14003（base 60s）——不得把 30m 压回 60s。
	p.CooldownModelRateLimit("u1", time.Minute, "hy4-preview", "14003 model rate limit")

	p.mu.RLock()
	mc := p.byUID["u1"].modelCooldowns["hy4-preview"]
	p.mu.RUnlock()
	if d := time.Until(mc.Until); d < 25*time.Minute {
		t.Errorf("Until 距 now=%v，应保留原 30m 冷却（不得被 60s 缩短）", d)
	}
}

// TestCooldownModelRateLimitCappedBySoftRateMax base 超过 soft_rate_max 时截断。
func TestCooldownModelRateLimitCappedBySoftRateMax(t *testing.T) {
	p := New("")
	p.SetSoftRateMax(2 * time.Minute)
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownModelRateLimit("u1", time.Hour, "hy4-preview", "14003 model rate limit")

	p.mu.RLock()
	mc := p.byUID["u1"].modelCooldowns["hy4-preview"]
	p.mu.RUnlock()
	if d := time.Until(mc.Until); d > 3*time.Minute {
		t.Errorf("Until 距 now=%v，应被 soft_rate_max(2m) 截断", d)
	}
}

// TestCooldownModelRateLimitKeeps11102Reason 既有 11102 负缓存的 reason 前缀与 Hits
// 不被 14003 覆盖（BlockModelClear 靠 "11102" 前缀识别，覆盖后该模型永远清不掉）。
func TestCooldownModelRateLimitKeeps11102Reason(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.BlockModelBackoff("u1", "hy4-preview", "11102 model not available")
	p.CooldownModelRateLimit("u1", time.Minute, "hy4-preview", "14003 model rate limit")

	p.mu.RLock()
	mc := p.byUID["u1"].modelCooldowns["hy4-preview"]
	p.mu.RUnlock()
	if !strings.HasPrefix(mc.Reason, "11102") {
		t.Errorf("reason=%q 应保留 11102 前缀", mc.Reason)
	}
	if mc.Hits == 0 {
		t.Error("Hits 应保留 11102 的累计命中数")
	}
}

// TestCooldownModelRateLimitEmptyArgsNoop 空 uid/model 无副作用（不误冻账号）。
func TestCooldownModelRateLimitEmptyArgsNoop(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownModelRateLimit("", time.Minute, "hy4-preview", "14003 model rate limit")
	p.CooldownModelRateLimit("u1", time.Minute, "", "14003 model rate limit")

	p.mu.RLock()
	e := p.byUID["u1"]
	n := len(e.modelCooldowns)
	until := e.until
	p.mu.RUnlock()
	if n != 0 || !until.IsZero() {
		t.Errorf("空参应无副作用：modelCooldowns=%d until=%v", n, until)
	}
}

// TestCooldownModelRateLimitIndependentPerModel 同号两个模型各自独立冷却（14003 语义）。
func TestCooldownModelRateLimitIndependentPerModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownModelRateLimit("u1", 2*time.Minute, "hy4-preview", "14003 model rate limit")
	p.CooldownModelRateLimit("u1", time.Minute, "hy4-preview-f", "14003 model rate limit")

	p.mu.RLock()
	e := p.byUID["u1"]
	a := e.modelCooldowns["hy4-preview"]
	b := e.modelCooldowns["hy4-preview-f"]
	until := e.until
	p.mu.RUnlock()

	if !until.IsZero() {
		t.Errorf("until=%v 应为零值（两次模型级限流都不得写账号级）", until)
	}
	if d := time.Until(a.Until) - time.Until(b.Until); d < 40*time.Second {
		t.Errorf("两模型冷却差=%v，应各按自身 base 独立计时（≈60s 差）", d)
	}
}
