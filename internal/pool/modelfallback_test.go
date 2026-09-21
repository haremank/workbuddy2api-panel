package pool

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 独立审核（round9）发现的缺陷：**模型级冷却覆盖该 realm 全部账号时，选号直接返回 nil**
// ⇒ 客户端拿到即时 503，**一次上游尝试都不发**，而该模型仍列在 /v1/models 里。
//
// 根因：pickEarliestExpiryLocked（全冷却兜底）用 `e.expiry(now)` 判断"是否在冷却期"，
// 而 expiry() 只覆盖账号级三维（until / breakerUntil / degradeUntil），**不含
// modelCooldowns** ⇒ 账号本身健康、只是"该模型"被限流的，expiry 返回零值 → 被兜底跳过
// ⇒ best == nil → 503。
//
// 与函数自己的契约不符：注释写的是「无 healthy 候选时，从冷却账号里选 until 最早到期的
// 一个」—— 模型级冷却也是"冷却"，也该有半开试探的机会。
//
// 实测现场（2026-09-21 23:22，线上）：`global:hy4-*` 连续即时 503（uid=- total=0.0s），
// 而同一时刻 `global:deep-model` 正常 200 ⇒ 排除纯粹来自模型级冷却。
func TestFallbackCoversAllModelCooledAccounts(t *testing.T) {
	// 抓标准日志：兜底唯一的外部可观测痕迹是 `pool: fallback_earliest_expiry … kind=…`。
	// 没有这条断言，本用例会**空转** —— 正常选号若没被模型级冷却挡住，下面同样会返回非 nil，
	// 测试通过但什么都没证明。
	var buf bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prevOut)

	p := New("")
	for _, uid := range []string{"g1", "g2"} {
		p.Add(&auth.Auth{UID: uid, Domain: "www.workbuddy.ai"})
	}
	// 账号本身完全健康，只是"该模型"在限流冷却中（14003 的形态，分钟级）。
	for _, uid := range []string{"g1", "g2"} {
		p.CooldownModelRateLimit(uid, time.Minute, "m-cooled", "14003 probe model rate limit")
	}

	// 前提校验：不指定模型时账号可选 ⇒ 证明"不可选"确实只来自模型级冷却。
	if got := p.PickExcludingForRealm(nil, "", "global"); got == nil {
		t.Fatal("账号级健康：不指定模型时应可选（说明本用例构造有误）")
	}

	buf.Reset()
	got := p.PickExcludingForRealm(nil, "m-cooled", "global")
	logs := buf.String()
	if got == nil {
		t.Fatalf("BUG：该 realm 全部账号只是'该模型'被限流冷却，兜底却返回 nil "+
			"⇒ 客户端即时 503 且零上游尝试（模型列在 /v1/models 里却完全不可达）。日志：%s", logs)
	}
	if got.Realm() != "global" {
		t.Errorf("兜底跨域了：拿到 %s（realm=%s）", got.UID, got.Realm())
	}
	if !strings.Contains(logs, "fallback_earliest_expiry") {
		t.Errorf("必须走**全冷却兜底**（正常选号应被模型级冷却挡住），日志无该行：%s", logs)
	}
	if !strings.Contains(logs, "kind=model") {
		t.Errorf("兜底原因应标注为模型级（kind=model），实际日志：%s", logs)
	}
}

// 两梯队的边界（round9 的核心设计）：**第一梯队（账号级冷却）存在时，第二梯队不得抢**。
// 这条优先级是既有约定（TestHealthyForModelPriorityViaPick 钉住），第二梯队只在第一梯队
// 为空时启用 —— 否则就是"为了修 A 而改了 B"。
func TestFallbackTiersAccountCooledWinsOverModelCooled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "acct-cooled", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "model-cooled", Domain: "www.workbuddy.ai"})
	p.SetCredits("acct-cooled", 100, 0)
	p.SetCredits("model-cooled", 100, 0)
	p.Cooldown("acct-cooled", CoolSoft, time.Hour, "429")               // 账号级，1h
	p.CooldownModelRateLimit("model-cooled", time.Minute, "m", "14003") // 仅该模型，1min

	got := p.PickExcludingForRealm(nil, "m", "global")
	if got == nil {
		t.Fatal("两梯队都应有候选，不该 nil")
	}
	if got.UID != "acct-cooled" {
		t.Fatalf("第一梯队（账号级冷却）应优先，却选了 %s —— 梯队顺序被破坏", got.UID)
	}
}

// 反面：11102 负缓存（「该后端无此模型」的确定性答复）**不应**被兜底捞回来试探 ——
// 那正是负缓存存在的意义（避免反复打一个不存在的模型）。允许它试探等于把
// 6~24h 的退避机制废掉。
func TestFallbackSkipsModelBlockedAccounts(t *testing.T) {
	p := New("")
	for _, uid := range []string{"g1", "g2"} {
		p.Add(&auth.Auth{UID: uid, Domain: "www.workbuddy.ai"})
	}
	for _, uid := range []string{"g1", "g2"} {
		p.BlockModelBackoff(uid, "m-gone", "11102 model not available")
	}
	if got := p.PickExcludingForRealm(nil, "m-gone", "global"); got != nil {
		t.Errorf("11102 负缓存条目不该被兜底试探，却选中了 %s", got.UID)
	}
}

// 兜底不得跨域：只有 global 号被模型级冷却时，cn 请求不能因此拿到 global 号。
func TestFallbackModelCooldownStaysInRealm(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "c1", Domain: "www.codebuddy.cn"})
	p.CooldownModelRateLimit("g1", time.Minute, "m", "14003")
	p.CooldownModelRateLimit("c1", time.Minute, "m", "14003")

	got := p.PickExcludingForRealm(nil, "m", "cn")
	if got == nil {
		t.Fatal("cn 兜底应给出 cn 号")
	}
	if got.Realm() != "cn" {
		t.Fatalf("cn 兜底拿到 %s（realm=%s）—— 跨域", got.UID, got.Realm())
	}
}

// 回归：余额已确认耗尽的号仍然不得参与兜底（调了必 14018）。
func TestFallbackStillExcludesCreditExhausted(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "g2", Domain: "www.workbuddy.ai"})
	p.SetCredits("g1", 0, 100)
	p.SetCredits("g2", 0, 100)
	for _, uid := range []string{"g1", "g2"} {
		p.CooldownModelRateLimit(uid, time.Minute, "m", "14003")
	}
	if got := p.PickExcludingForRealm(nil, "m", "global"); got != nil {
		t.Errorf("余额为 0 的号不该参与兜底，却选中了 %s", got.UID)
	}
}
