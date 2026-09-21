package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// switcherEntry 一条切换器导出形条目（wb-switch 等桌面端导出的字段名）。
const switcherEntry = `{
  "access_token": "snake-access",
  "auth_raw": {
    "accessToken": "raw-access",
    "refreshToken": "raw-refresh",
    "domain": "www.workbuddy.ai",
    "expiresIn": 29743626,
    "scope": "openid profile offline_access email",
    "tokenType": "Bearer"
  },
  "createdAt": 1789698708401,
  "domain": "www.workbuddy.ai",
  "email": null,
  "expiresAt": 1819442363020,
  "id": "c5a2c076-be57-4ee2-8c3c-3986affc3360",
  "nickname": "haremank",
  "profile_raw": {"nickname": "haremank", "type": "personal", "uid": "e5ecbb8b-3051-4291-9cb7-52c8273021e7"},
  "refresh_token": "snake-refresh",
  "refreshedAt": 1789916811020,
  "uid": "e5ecbb8b-3051-4291-9cb7-52c8273021e7",
  "variant": "ai"
}`

// TestImportSwitcherShape 切换器导出形必须归一化为嵌套形凭证：
// auth_raw 优先于顶层 snake_case、毫秒时间戳折成秒、domain 判出 global 域。
func TestImportSwitcherShape(t *testing.T) {
	items, err := parseImportItems([]byte("[" + switcherEntry + "]"))
	if err != nil {
		t.Fatalf("parseImportItems: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %d", len(items))
	}
	a, err := toAuth(items[0])
	if err != nil {
		t.Fatalf("toAuth: %v", err)
	}
	if a.AccessToken != "raw-access" {
		t.Errorf("AccessToken = %q, want auth_raw.accessToken（原始 OAuth 响应优先）", a.AccessToken)
	}
	if a.RefreshToken != "raw-refresh" {
		t.Errorf("RefreshToken = %q", a.RefreshToken)
	}
	if a.UID != "e5ecbb8b-3051-4291-9cb7-52c8273021e7" {
		t.Errorf("UID = %q", a.UID)
	}
	if a.Nickname != "haremank" {
		t.Errorf("Nickname = %q", a.Nickname)
	}
	if a.Domain != "www.workbuddy.ai" {
		t.Errorf("Domain = %q", a.Domain)
	}
	if want := int64(1819442363); a.ExpiresAt != want {
		t.Errorf("ExpiresAt = %d, want %d（毫秒 → 秒）", a.ExpiresAt, want)
	}
	if got := a.Realm(); got != "global" {
		t.Errorf("Realm() = %q, want global（domain 后缀 .workbuddy.ai）", got)
	}
}

// TestImportExpiresFallback 无 expiresAt 时用 refreshedAt + expiresIn 兜底。
func TestImportExpiresFallback(t *testing.T) {
	it := importItem{
		AccessTokenSnake: "t",
		UID:              "u1",
		RefreshedAt:      1789916811020, // 毫秒
		AuthRaw:          json.RawMessage(`{"accessToken":"t","expiresIn":3600}`),
	}
	a, err := toAuth(it)
	if err != nil {
		t.Fatalf("toAuth: %v", err)
	}
	if want := int64(1789916811 + 3600); a.ExpiresAt != want {
		t.Errorf("ExpiresAt = %d, want %d", a.ExpiresAt, want)
	}
}

// TestImportNativeNested 原生嵌套形（auths/*.json 数组）走 auth.Parse 路径。
func TestImportNativeNested(t *testing.T) {
	raw := `[{"auth":{"accessToken":"n-access","refreshToken":"n-refresh","expiresAt":1900000000,
	          "domain":"copilot.tencent.com","realm":"cn"},
	         "account":{"uid":"u-cn","enterpriseId":"","nickname":"CN 号"}}]`
	items, err := parseImportItems([]byte(raw))
	if err != nil {
		t.Fatalf("parseImportItems: %v", err)
	}
	a, err := toAuth(items[0])
	if err != nil {
		t.Fatalf("toAuth: %v", err)
	}
	if a.AccessToken != "n-access" || a.UID != "u-cn" || a.Nickname != "CN 号" {
		t.Errorf("嵌套形解析错误: %+v", a)
	}
	if a.ExpiresAt != 1900000000 {
		t.Errorf("ExpiresAt = %d, want 1900000000（秒级不得被当毫秒折算）", a.ExpiresAt)
	}
	if got := a.Realm(); got != "cn" {
		t.Errorf("Realm() = %q, want cn", got)
	}
}

// TestImportRejects 缺 token / 缺 uid / uid 含路径字符（路径穿越）一律拒绝。
func TestImportRejects(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"缺 accessToken", `[{"uid":"u1","refreshToken":"r"}]`},
		{"缺 uid", `[{"access_token":"a"}]`},
		{"uid 路径穿越", `[{"access_token":"a","uid":"../../evil"}]`},
		{"空数组", `[]`},
		{"非法 JSON", `{`},
		{"顶层为标量", `"x"`},
	}
	for _, c := range cases {
		items, err := parseImportItems([]byte(c.body))
		if err == nil {
			_, err = toAuth(items[0])
		}
		if err == nil {
			t.Errorf("%s: 期望报错，实际通过", c.name)
		}
	}
}

// TestImportAccountsWrapper 支持 {"accounts":[...]} 包裹形。
func TestImportAccountsWrapper(t *testing.T) {
	items, err := parseImportItems([]byte(`{"accounts":[` + switcherEntry + `]}`))
	if err != nil {
		t.Fatalf("parseImportItems: %v", err)
	}
	if len(items) != 1 || items[0].UID == "" {
		t.Fatalf("包裹形解析失败: %+v", items)
	}
}

// TestAccountsImportHandler 端到端：落盘（0600 嵌套形）+ 进池 + 重复导入标 updated
// + dry_run 不落盘。
func TestAccountsImportHandler(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), AuthDir: dir})

	post := func(body, query string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/panel/api/accounts/import"+query, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer k")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}

	// 1) dry_run：只校验，不落盘、不进池。
	rec := post("["+switcherEntry+"]", "?dry_run=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("dry_run 状态码 = %d，body=%s", rec.Code, rec.Body.String())
	}
	var dr map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &dr)
	if dr["added"].(float64) != 1 || dr["dry_run"] != true {
		t.Fatalf("dry_run 结果异常: %s", rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "workbuddy-e5ecbb8b-3051-4291-9cb7-52c8273021e7.json")); !os.IsNotExist(err) {
		t.Fatalf("dry_run 不应落盘")
	}

	// 2) 真导入：落盘 + 进池。
	rec = post("["+switcherEntry+"]", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("导入状态码 = %d，body=%s", rec.Code, rec.Body.String())
	}
	path := filepath.Join(dir, "workbuddy-e5ecbb8b-3051-4291-9cb7-52c8273021e7.json")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("凭证文件未落盘: %v", err)
	}
	// Windows 不做 POSIX 权限位映射（可写文件一律报 0666），仅在类 Unix 上断言 0600。
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("凭证文件权限 = %o, want 600", fi.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"auth"`) || !strings.Contains(string(raw), `"account"`) {
		t.Errorf("落盘应为嵌套形: %s", raw)
	}
	if _, ok := p.cfg.Pool.Status("e5ecbb8b-3051-4291-9cb7-52c8273021e7"); !ok {
		t.Fatalf("账号未进池")
	}
	// 落盘文件的 realm 必须显式为 global（不依赖 domain 回落）。
	if a, err := auth.Parse(raw); err != nil || a.RealmStored() != "global" {
		t.Errorf("落盘 realm 标识异常: err=%v stored=%q", err, a.RealmStored())
	}

	// 3) 重复导入 → updated（覆盖凭证，不重复计新增）。
	rec = post("["+switcherEntry+"]", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &dr)
	if dr["updated"].(float64) != 1 || dr["added"].(float64) != 0 {
		t.Fatalf("重复导入应标 updated: %s", rec.Body.String())
	}

	// 4) 单条失败不影响同批其它条目。
	rec = post(`[{"access_token":"a","uid":"../../evil"},{"access_token":"b","uid":"u2"}]`, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &dr)
	if dr["failed"].(float64) != 1 || dr["added"].(float64) != 1 {
		t.Fatalf("混合批次计数异常: %s", rec.Body.String())
	}
}

// TestImportHandlerRequiresAuth 导入接口与其它 /panel/api/* 同口径：无 key 401。
func TestImportHandlerRequiresAuth(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), AuthDir: t.TempDir()})
	req := httptest.NewRequest("POST", "/panel/api/accounts/import", strings.NewReader("[]"))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无 key 状态码 = %d, want 401", rec.Code)
	}
}

// TestNormEpoch 时间戳阈值：秒级不得被误折。
func TestNormEpoch(t *testing.T) {
	now := time.Now().Unix()
	if got := normEpoch(now); got != now {
		t.Errorf("秒级被误折: %d → %d", now, got)
	}
	if got := normEpoch(now * 1000); got != now {
		t.Errorf("毫秒未归一: %d → %d, want %d", now*1000, got, now)
	}
}
