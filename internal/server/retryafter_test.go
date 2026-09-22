package server

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// fakeUpstream429 返回一个对所有请求都回 429 的 fake upstream，并带上给定响应头。
// newFakeUpstream 不支持自定义头（它固定只给 Content-Type），而 Retry-After 用例
// 的核心就是响应头，故单独建一个。
func fakeUpstream429(t *testing.T, hdr http.Header) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			h := http.Header{"Content-Type": []string{"application/json"}}
			for k, vs := range hdr {
				h[k] = vs
			}
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     h,
				Body:       io.NopCloser(strings.NewReader(`{"code":1,"msg":"rate limit"}`)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// TestChat429CarriesUpstreamRetryAfter（N2 端到端）：上游明示了 Retry-After → 客户端
// 必须拿到该值。此前该头只被用作**冷却时长**，从不回客户端 ⇒ OpenAI SDK 等拿不到退避
// 依据，只能按自身经验值立刻重试，反复撞穿限流窗口。
//
// ⚠️ 本用例**测不出"上游优先于池内"这条优先级**（差点写成假声明，被变异测试抓出）：
// applyErrorPolicy 会把上游 Retry-After 直接用作本号冷却时长（handler.go「429 rate
// limit (retry-after)」分支），于是池内推导值与上游值**天然相等**。优先级由
// TestSetRetryAfterUpstreamWinsOverPool 单独钉住（那里池内 1h vs 上游 37s 可分辨）。
// 本用例的价值是端到端证明"429 路径上确实会发出该头且值正确"。
func TestChat429CarriesUpstreamRetryAfter(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{
		Pool:         p,
		Upstream:     fakeUpstream429(t, http.Header{"Retry-After": []string{"7"}}),
		SoftCooldown: time.Minute,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s want 429", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Retry-After"); got != "7" {
		t.Errorf("Retry-After=%q want 7（上游明示值须原样回传；优先级判别见 TestSetRetryAfterUpstreamWinsOverPool）", got)
	}
}

// TestChat429DerivesRetryAfterFromPool：上游**没给**该头时，用池内最早恢复时刻兜底
// ——否则客户端只能瞎猜。断言方式刻意绑到池自己的状态（CoolRemaining）而非硬编码数字：
// 硬编码 60 会在软冷却基数/退避策略调整时变成假绿。
func TestChat429DerivesRetryAfterFromPool(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{
		Pool:         p,
		Upstream:     fakeUpstream429(t, nil), // 上游不给 Retry-After
		SoftCooldown: time.Minute,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s want 429", rec.Code, rec.Body)
	}
	// 前提校验：429 后该号处于软冷却 —— 兜底路径才有值可取。
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatalf("前提失败：429 后账号应处于软冷却，got %+v", st)
	}
	raw := rec.Header().Get("Retry-After")
	got, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("Retry-After=%q 缺失或非整数（上游没给头时应由池内最早恢复时刻兜底）", raw)
	}
	if diff := st.CoolRemaining - int64(got); diff < 0 || diff > 2 {
		t.Errorf("Retry-After=%d 与池内剩余冷却 %d 不一致（两者必须同源）", got, st.CoolRemaining)
	}
}

// TestSetRetryAfterOmitsWhenNothingKnown：上游没给头 + 池内无可折算状态 → 必须**不发**
// 该头（不编数字）。编一个数字比不发更糟：客户端会在该重试时放弃，或过早重试继续撞限流。
//
// 这里直接调 helper 而不是走端到端：429 路径上 applyErrorPolicy 总会把撞限流的号软冷却，
// 端到端构造不出"上游与池内都缺"的现场（那条分支只能由单测钉住）。
func TestSetRetryAfterOmitsWhenNothingKnown(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p})

	rec := httptest.NewRecorder()
	h.setRetryAfter(rec, 0, "cn", "glm-5.2")
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("两者都缺时必须不发头，got %q", got)
	}
}

// TestSetRetryAfterUpstreamWinsOverPool：池内正冷却着（1h）但上游给了值 → 用上游的。
// 上游是权威（它知道自己那边的窗口），池内推导只是兜底。
func TestSetRetryAfterUpstreamWinsOverPool(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.Cooldown("u1", pool.CoolSoft, time.Hour, "429 rate limit")
	h := NewHandler(Config{Pool: p})

	rec := httptest.NewRecorder()
	h.setRetryAfter(rec, 37*time.Second, "cn", "glm-5.2")
	if got := rec.Header().Get("Retry-After"); got != "37" {
		t.Errorf("上游明示值优先于池内 1h，got %q want 37", got)
	}
}

// TestSetRetryAfterLogsSource：发出该头时必须打一行日志（运维可观测 + 部署靶心标记）。
// 没有这条断言，日志行被删掉不会有任何测试变红，而线上靶心验证会莫名失败
// （round14 的靶心就是在容器二进制里 grep 该标记）。
func TestSetRetryAfterLogsSource(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.Cooldown("u1", pool.CoolSoft, 30*time.Second, "429 rate limit")
	h := NewHandler(Config{Pool: p})

	// ① 池内兜底 → src=pool。
	rec := httptest.NewRecorder()
	h.setRetryAfter(rec, 0, "cn", "glm-5.2")
	if !strings.Contains(buf.String(), "chat 429 retry-after=") {
		t.Errorf("缺日志标记（部署靶心依赖它）：%q", buf.String())
	}
	if !strings.Contains(buf.String(), "src=pool") {
		t.Errorf("应标明取值来源 src=pool：%q", buf.String())
	}

	// ② 上游明示优先 → src=upstream（池内那 30s 不生效）。
	buf.Reset()
	rec = httptest.NewRecorder()
	h.setRetryAfter(rec, 37*time.Second, "cn", "glm-5.2")
	if !strings.Contains(buf.String(), "src=upstream") {
		t.Errorf("上游给了值时应记 src=upstream：%q", buf.String())
	}

	// ③ 不发头 → 不打日志（否则日志会被无意义的 429 刷屏）。
	buf.Reset()
	h2 := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "ok", AccessToken: "at", ExpiresAt: 9999999999})})
	rec = httptest.NewRecorder()
	h2.setRetryAfter(rec, 0, "cn", "glm-5.2")
	if buf.Len() != 0 {
		t.Errorf("不发头时不该打日志：%q", buf.String())
	}
}

// TestRetryAfterHeaderValueCeils：秒数折算必须**向上取整**（RFC 7231 delta-seconds 是
// 整数秒）。截断会让客户端早不到 1 秒重试、又撞上限流窗口；非正值不发头。
func TestRetryAfterHeaderValueCeils(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
		ok   bool
	}{
		{0, "", false},
		{-time.Second, "", false},
		{time.Millisecond, "1", true},
		{time.Second, "1", true},
		{time.Second + time.Millisecond, "2", true}, // 向上取整
		{37 * time.Second, "37", true},
		{60*time.Second - time.Millisecond, "60", true},
	}
	for _, c := range cases {
		got, ok := retryAfterHeaderValue(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("retryAfterHeaderValue(%v) = (%q,%v) want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
