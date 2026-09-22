// import.go 面板「JSON 导入账号」：把外部导出的凭证 JSON 批量归一化成 auths/ 下的
// 标准凭证文件，并热加载进池（免重启，与 loginPoll 同路径）。
//
// 支持三种输入形态，自动识别，同一文件内可混装：
//
//  1. 原生嵌套形  [{"auth":{"accessToken":...},"account":{"uid":...}}, ...]
//     —— 即 auths/*.json 直接拼成数组，或 {"accounts":[...]} 包一层
//  2. 原生扁平形  [{"accessToken":...,"refreshToken":...,"uid":...,"expiresAt":秒}, ...]
//  3. 切换器导出形 [{access_token, refresh_token, auth_raw:{accessToken,refreshToken,
//     domain,expiresIn}, uid, nickname, expiresAt(毫秒), profile_raw:{...}}, ...]
//     —— 桌面端账号切换器（wb-switch 等）的导出格式
//
// 落盘一律归一化为嵌套形（与 login.sh / loginPoll / SaveAtomic 同形），文件名
// workbuddy-<uid>.json；uid 经 validUID 校验后才用于拼路径（防路径穿越）。
package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// importMaxBytes 导入请求体上限。账号导出文件通常几十 KB，8MB 足够且防滥用。
const importMaxBytes = 8 << 20

// msEpochThreshold 毫秒时间戳判定阈值：当前秒级时间戳约 1.8e9、毫秒级约 1.8e12，
// 1e11 落在两者之间且留足余量（秒级要到公元 5138 年才越过）。外部导出普遍用毫秒，
// 本服务内部一律秒，导入时按此归一。
const msEpochThreshold = 1e11

// importItem 外部条目字段并集（三种形态的键全收，缺失即空）。识别顺序见 toAuth。
type importItem struct {
	// —— 切换器导出形（snake_case 顶层 + auth_raw 原始 OAuth 响应）——
	AccessTokenSnake  string          `json:"access_token"`
	RefreshTokenSnake string          `json:"refresh_token"`
	AuthRaw           json.RawMessage `json:"auth_raw"`
	ProfileRaw        json.RawMessage `json:"profile_raw"`
	Variant           string          `json:"variant"`
	RefreshedAt       int64           `json:"refreshedAt"` // 毫秒
	Email             string          `json:"email"`
	ID                string          `json:"id"` // 导出条目的本地 id（非 uid，仅兜底）

	// —— 原生形（camelCase 扁平 / 嵌套）——
	AccessToken  string          `json:"accessToken"`
	RefreshToken string          `json:"refreshToken"`
	ExpiresIn    int64           `json:"expiresIn"` // 秒（相对）
	ExpiresAt    int64           `json:"expiresAt"` // 秒或毫秒（按阈值归一）
	Domain       string          `json:"domain"`
	Realm        string          `json:"realm"`
	UID          string          `json:"uid"`
	EnterpriseID string          `json:"enterpriseId"`
	Nickname     string          `json:"nickname"`
	DeviceToken  string          `json:"device_token"`
	Auth         json.RawMessage `json:"auth"`
	Account      json.RawMessage `json:"account"`
}

// normEpoch 归一化时间戳为秒：> msEpochThreshold 视为毫秒。
func normEpoch(v int64) int64 {
	if v > msEpochThreshold {
		return v / 1000
	}
	return v
}

// isSwitcherShape 判定条目是否为「切换器导出形」：带 auth_raw 或顶层 snake_case token。
func isSwitcherShape(it importItem) bool {
	return len(it.AuthRaw) > 0 || strings.TrimSpace(it.AccessTokenSnake) != ""
}

// toAuth 把一条外部条目归一化成 auth.Auth（不做落盘；FilePath 由调用方按 uid 拼）。
// 返回 error 时该条跳过并在结果里报告，不影响同批其它条目。
func toAuth(it importItem) (*auth.Auth, error) {
	if !isSwitcherShape(it) {
		// 原生形：交给 auth.Parse 处理（它自带嵌套/扁平双形态识别与校验）。
		raw, err := json.Marshal(it)
		if err != nil {
			return nil, fmt.Errorf("条目序列化失败: %w", err)
		}
		a, err := auth.Parse(raw)
		if err != nil {
			return nil, err
		}
		return a, nil
	}

	// 切换器导出形：auth_raw 是原始 OAuth 响应，优先于顶层字段。
	access := strings.TrimSpace(it.AccessTokenSnake)
	refresh := it.RefreshTokenSnake
	domain := it.Domain
	expiresIn := int64(0)
	if len(it.AuthRaw) > 0 {
		var ar struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			Domain       string `json:"domain"`
			ExpiresIn    int64  `json:"expiresIn"`
		}
		if err := json.Unmarshal(it.AuthRaw, &ar); err != nil {
			return nil, fmt.Errorf("auth_raw 解析失败: %w", err)
		}
		if s := strings.TrimSpace(ar.AccessToken); s != "" {
			access = s
		}
		if ar.RefreshToken != "" {
			refresh = ar.RefreshToken
		}
		if ar.Domain != "" {
			domain = ar.Domain
		}
		expiresIn = ar.ExpiresIn
	}

	uid := strings.TrimSpace(it.UID)
	nickname := strings.TrimSpace(it.Nickname)
	if len(it.ProfileRaw) > 0 {
		var pr struct {
			UID      string `json:"uid"`
			Nickname string `json:"nickname"`
		}
		if err := json.Unmarshal(it.ProfileRaw, &pr); err == nil {
			if uid == "" {
				uid = strings.TrimSpace(pr.UID)
			}
			if nickname == "" {
				nickname = strings.TrimSpace(pr.Nickname)
			}
		}
	}
	if uid == "" {
		// 兜底：导出的本地 id 实测就是账号 UUID，但不保证，仅在没有更权威来源时用。
		uid = strings.TrimSpace(it.ID)
	}
	if nickname == "" {
		nickname = strings.TrimSpace(it.Email)
	}

	// 过期时刻优先级：显式 expiresAt（归一毫秒）→ refreshedAt + expiresIn → now + expiresIn。
	expiresAt := normEpoch(it.ExpiresAt)
	if expiresAt <= 0 && expiresIn > 0 {
		base := normEpoch(it.RefreshedAt)
		if base <= 0 {
			base = time.Now().Unix()
		}
		expiresAt = base + expiresIn
	}

	a := &auth.Auth{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresAt:    expiresAt,
		Domain:       domain,
		UID:          uid,
		EnterpriseID: strings.TrimSpace(it.EnterpriseID),
		Nickname:     nickname,
		DeviceToken:  strings.TrimSpace(it.DeviceToken),
	}
	if strings.TrimSpace(access) == "" {
		return nil, fmt.Errorf("缺少 access token")
	}
	if uid == "" {
		return nil, fmt.Errorf("缺少 uid（无法确定凭证文件名）")
	}
	if !validUID(uid) {
		return nil, fmt.Errorf("uid 含非法字符，拒绝落盘（防路径穿越）")
	}
	// realm 标识：显式 cn/global 优先，否则按 domain 推断（与 LoadDir 存量迁移同口径）。
	if _, err := auth.BackfillRealmFor(a, auth.ResolveRealm(it.Realm, domain)); err != nil {
		return nil, err
	}
	return a, nil
}

// parseImportItems 解析导入请求体为条目列表。接受：
//
//	[ ... ]                     数组
//	{"accounts":[ ... ]}        带 accounts 包裹
//	{ ... }                     单条
//
// 空数组/空对象/非法 JSON 均报错（由 handler 转 400）。
func parseImportItems(raw []byte) ([]importItem, error) {
	raw = bytes.TrimSpace(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")))
	if len(raw) == 0 {
		return nil, fmt.Errorf("请求体为空")
	}
	switch raw[0] {
	case '[':
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil, fmt.Errorf("JSON 数组解析失败: %w", err)
		}
		return decodeItems(arr)
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("JSON 对象解析失败: %w", err)
		}
		if inner, ok := obj["accounts"]; ok {
			var arr []json.RawMessage
			if err := json.Unmarshal(inner, &arr); err != nil {
				return nil, fmt.Errorf("accounts 字段不是数组: %w", err)
			}
			return decodeItems(arr)
		}
		return decodeItems([]json.RawMessage{raw})
	default:
		return nil, fmt.Errorf("无法识别的 JSON 顶层结构（应为数组或对象）")
	}
}

// decodeItems 逐条解码，跳过 null / 空对象。
func decodeItems(raws []json.RawMessage) ([]importItem, error) {
	out := make([]importItem, 0, len(raws))
	for i, r := range raws {
		t := bytes.TrimSpace(r)
		if len(t) == 0 || string(t) == "null" {
			continue
		}
		var it importItem
		if err := json.Unmarshal(t, &it); err != nil {
			return nil, fmt.Errorf("第 %d 条解析失败: %w", i+1, err)
		}
		out = append(out, it)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("未发现任何账号条目")
	}
	return out, nil
}

// accountsImport 批量导入账号凭证。
//
//	POST /panel/api/accounts/import?dry_run=1   （dry_run 只校验不落盘，供前端预览）
//
// 行为：逐条归一化 → 校验 uid/token → 原子落盘 auths/workbuddy-<uid>.json →
// 热加载进池（Add + Revive，与全新登录同口径：清掉旧号遗留的禁用/冷却/熔断）。
// 同 uid 已存在时覆盖凭证文件并更新池内条目，结果标为 updated。
// 单条失败不影响同批其它条目，失败原因逐条返回（错误信息不含任何 token）。
func (p *Panel) accountsImport(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, importMaxBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读取请求体失败: "+err.Error())
		return
	}
	if len(raw) > importMaxBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "请求体过大（上限 8MB）")
		return
	}
	dryRun := isTruthy(r.URL.Query().Get("dry_run"))

	items, err := parseImportItems(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	type row struct {
		UID      string `json:"uid,omitempty"`
		Nickname string `json:"nickname,omitempty"`
		Realm    string `json:"realm,omitempty"`
		Status   string `json:"status"` // added / updated / error
		Error    string `json:"error,omitempty"`
	}
	rows := make([]row, 0, len(items))
	added, updated, failed := 0, 0, 0

	for _, it := range items {
		a, err := toAuth(it)
		if err != nil {
			failed++
			rows = append(rows, row{Status: "error", Error: err.Error()})
			continue
		}
		// 展示用域标识：a.realm 已由 Parse（原生形）或 BackfillRealmFor（导出形）填好，
		// 为空时 Realm() 按 domain 后缀回落——三形态统一口径。
		realm := a.Realm()

		_, existed := p.cfg.Pool.Status(a.UID)
		filePath := filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", a.UID))
		if !existed {
			if _, err := os.Stat(filePath); err == nil {
				existed = true // 文件在但没进池（例如刚手工放入、尚未重启）→ 也算覆盖
			}
		}
		status := "added"
		if existed {
			status = "updated"
		}

		if !dryRun {
			if err := os.MkdirAll(p.cfg.AuthDir, 0o755); err != nil {
				failed++
				rows = append(rows, row{UID: a.UID, Status: "error", Error: "创建 auth 目录失败: " + err.Error()})
				continue
			}
			a.FilePath = filePath
			if err := a.SaveAtomic(); err != nil {
				failed++
				rows = append(rows, row{UID: a.UID, Nickname: a.Nickname, Realm: realm, Status: "error",
					Error: "写入凭证失败: " + err.Error()})
				continue
			}
			p.cfg.Pool.Add(a)
			p.cfg.Pool.Revive(a.UID) // 与全新登录同口径：人工恢复，清禁用/冷却/熔断
		}

		if existed {
			updated++
		} else {
			added++
		}
		rows = append(rows, row{UID: a.UID, Nickname: a.Nickname, Realm: realm, Status: status})
	}

	log.Printf("panel: JSON 导入 %d 条（新增 %d / 覆盖 %d / 失败 %d）dry_run=%v",
		len(items), added, updated, failed, dryRun)
	if !dryRun {
		// 只打 uid 前 8 位，避免长 uid 刷屏；不含任何 token。
		for _, it := range rows {
			if it.Status == "error" {
				log.Printf("panel: 导入跳过 %s: %s", logfmt.UID8(it.UID), it.Error)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"dry_run":  dryRun,
		"total":    len(items),
		"added":    added,
		"updated":  updated,
		"failed":   failed,
		"accounts": rows,
	})
}

// isTruthy 宽松解析布尔查询参数（1/true/yes/on）。
func isTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
