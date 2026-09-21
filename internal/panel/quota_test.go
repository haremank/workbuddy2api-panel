package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// TestQuotaRealmAggregation 额度水位接口：按 realm 汇总，并逐号透出余额三态判据。
//
// 背景（2026-09-21 诊断）：上游对余额耗尽账号回 429 + code 14018（Credits exhausted），
// 修复前被判 ErrSoftRate 只软冷却 600s ⇒ 0 额度号反复被选中且每次必失败。本接口
// 是"看得见"的那一半：0 额度号、濒危号、读数未知号必须能被区分出来。
func TestQuotaRealmAggregation(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "cn-rich"})                                // 国内版，余额充足
	pl.Add(&auth.Auth{UID: "cn-drained"})                             // 国内版，已耗尽
	pl.Add(&auth.Auth{UID: "gl-unknown", Domain: "www.workbuddy.ai"}) // 国际版，从未查到余额
	pl.SetCredits("cn-rich", 800, 1000)
	pl.SetCredits("cn-drained", 0, 500)

	p := New(Config{Version: "test", APIKey: "test-key", Pool: pl})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/api/quota", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var d struct {
		LowThreshold int64 `json:"low_threshold"`
		StaleSec     int64 `json:"stale_sec"`
		Realms       []struct {
			Realm    string `json:"realm"`
			Label    string `json:"label"`
			Accounts int    `json:"accounts"`
			Servable int    `json:"servable"`
			Zero     int    `json:"zero"`
			Unknown  int    `json:"unknown"`
			Credits  int64  `json:"credits"`
		} `json:"realms"`
		Accounts []struct {
			UID           string `json:"uid"`
			Realm         string `json:"realm"`
			Credits       int64  `json:"credits"`
			CreditsKnown  bool   `json:"credits_known"`
			Servable      bool   `json:"servable"`
			CreditsUpdate string `json:"credits_updated"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}

	if d.LowThreshold != lowCreditsDefault {
		t.Errorf("low_threshold=%d want %d", d.LowThreshold, lowCreditsDefault)
	}
	// 未注入 BalanceRefreshInterval → 回落 5 分钟默认值，过期判据 = 3 个周期 = 900s。
	if d.StaleSec != 900 {
		t.Errorf("stale_sec=%d want 900", d.StaleSec)
	}

	byRealm := map[string]int{}
	for _, r := range d.Realms {
		byRealm[r.Realm] = r.Accounts
		if r.Label == "" {
			t.Errorf("realm %s 缺 label", r.Realm)
		}
	}
	if byRealm["cn"] != 2 || byRealm["global"] != 1 {
		t.Fatalf("realm 分组错误: %v", byRealm)
	}

	var cn struct {
		zero, unknown, servable int
		credits                 int64
	}
	for _, r := range d.Realms {
		if r.Realm == "cn" {
			cn.zero, cn.unknown, cn.servable, cn.credits = r.Zero, r.Unknown, r.Servable, r.Credits
		}
	}
	if cn.zero != 1 {
		t.Errorf("cn.zero=%d want 1（cn-drained 已耗尽）", cn.zero)
	}
	if cn.unknown != 0 {
		t.Errorf("cn.unknown=%d want 0", cn.unknown)
	}
	if cn.servable != 1 {
		t.Errorf("cn.servable=%d want 1（耗尽号已出池）", cn.servable)
	}
	if cn.credits != 800 {
		t.Errorf("cn.credits=%d want 800", cn.credits)
	}

	for _, a := range d.Accounts {
		switch a.UID {
		case "cn-drained":
			if !a.CreditsKnown {
				t.Error("cn-drained credits_known=false，耗尽判据不成立")
			}
			if a.Servable {
				t.Error("cn-drained 余额为 0 却仍 servable —— 余额判据未生效")
			}
		case "cn-rich":
			if !a.Servable {
				t.Error("cn-rich 应 servable")
			}
			if a.CreditsUpdate == "" {
				t.Error("cn-rich 缺 credits_updated（额度水位视图的鲜度来源）")
			}
		case "gl-unknown":
			if a.Realm != "global" {
				t.Errorf("gl-unknown realm=%q want global", a.Realm)
			}
			if a.CreditsKnown {
				t.Error("gl-unknown 从未查到余额，credits_known 应为 false")
			}
			if !a.Servable {
				t.Error("gl-unknown 余额未知不应被出池（防余额查询故障误伤整池）")
			}
		default:
			t.Errorf("意外账号 %q", a.UID)
		}
	}
}

// TestQuotaLowThresholdOverride ?low=N 覆盖濒危阈值；非法值被忽略（回落默认）。
func TestQuotaLowThresholdOverride(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1"})
	pl.SetCredits("u1", 30, 100)
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pl})

	for _, c := range []struct {
		q    string
		want int64
	}{
		{"", lowCreditsDefault},
		{"?low=10", 10},
		{"?low=0", 0},
		{"?low=abc", lowCreditsDefault},
		{"?low=-5", lowCreditsDefault},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/panel/api/quota"+c.q, nil)
		req.Header.Set("Authorization", "Bearer test-key")
		p.ServeHTTP(rec, req)
		var d struct {
			LowThreshold int64 `json:"low_threshold"`
			Realms       []struct {
				Low int `json:"low"`
			} `json:"realms"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
			t.Fatalf("%s decode: %v", c.q, err)
		}
		if d.LowThreshold != c.want {
			t.Errorf("%s low_threshold=%d want %d", c.q, d.LowThreshold, c.want)
		}
		// 余额 30：阈值 10/0 时不算濒危；阈值 50（默认）时算。
		wantLow := 0
		if c.want == lowCreditsDefault {
			wantLow = 1
		}
		if len(d.Realms) != 1 || d.Realms[0].Low != wantLow {
			t.Errorf("%s realms.low=%v want %d", c.q, d.Realms, wantLow)
		}
	}
}
