package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// probeResp 预置响应。
type probeResp struct {
	status int
	body   string
}

// probeRoundTripper 假上游。
//
// chat 请求按请求体里的 model 名匹配 byModel；**无 model 的请求**（探针在 14018 后触发的
// 余额刷新走 billing 接口，body 里是 PageNumber/ProductCode 之类）走 nonChat。
// 不区分这两类会让"余额刷新"永远拿到 chat 的 SSE 响应 ⇒ 解析失败，测不出写回效果。
type probeRoundTripper struct {
	byModel map[string]probeResp
	nonChat *probeResp // nil = 200 + 空 SSE（等价于"余额查询失败"）

	// mu 保护 calls / nonChats：探针按 probeMaxConcurrency(=3) 并发跑，裸 append
	// 会丢记录（2026-09-22 全量测试偶发失败即此竞态）。测试侧一律经下方访问器读。
	mu       sync.Mutex
	calls    []string // 收到的 model 名（chat 请求）；非 chat 记 ""
	nonChats int      // 非 chat 请求计数

	// 并发观测（round10：探针改为串行，并发恒为 1）。**必须配 delay 使用** ——
	// 请求若瞬时返回，即便真的并发也会记录到峰值 1，断言就成了空转。
	inFlight    int
	maxInFlight int
	delay       time.Duration
}

// maxConcurrent 观测到的最大在途请求数（串行时应恒为 1）。
func (f *probeRoundTripper) maxConcurrent() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxInFlight
}

// callsSnapshot 返回已记录的 model 名副本（含非 chat 请求的空串）。并发安全。
func (f *probeRoundTripper) callsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// nonChatCount 返回非 chat 请求数（余额查询等）。并发安全。
func (f *probeRoundTripper) nonChatCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nonChats
}

func (f *probeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(r.Body)
	var req struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(raw, &req)

	if req.Model == "" {
		// 非 chat（余额查询等）
		f.mu.Lock()
		f.nonChats++
		f.calls = append(f.calls, "")
		f.mu.Unlock()
		if f.nonChat != nil {
			return mkProbeResp(*f.nonChat), nil
		}
		return mkProbeResp(probeResp{200, "data: {}\n\n"}), nil
	}

	f.mu.Lock()
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()
	if f.delay > 0 {
		time.Sleep(f.delay) // 让请求占用一段时间，把并发暴露出来（否则断言空转）
	}

	f.mu.Lock()
	f.calls = append(f.calls, req.Model)
	f.mu.Unlock()
	res, ok := f.byModel[req.Model]
	if !ok {
		res = probeResp{200, "data: {\"choices\":[]}\n\n"}
	}
	return mkProbeResp(res), nil
}

// mkProbeResp 把 probeResp 包成 http.Response（200 视为 SSE 流）。
func mkProbeResp(res probeResp) *http.Response {
	ct := "application/json"
	if res.status == 200 {
		ct = "text/event-stream"
	}
	return &http.Response{
		StatusCode: res.status,
		Header:     http.Header{"Content-Type": []string{ct}},
		Body:       io.NopCloser(strings.NewReader(res.body)),
	}
}

// newProbePanel 造一个面板 + 假上游 + 指定账号集合。
func newProbePanel(t *testing.T, fake *probeRoundTripper, accounts ...*auth.Auth) *Panel {
	t.Helper()
	auth.SetGlobalEnabled(true)
	pl := pool.New("")
	for _, a := range accounts {
		pl.Add(a)
	}
	hc := &http.Client{Transport: fake}
	return New(Config{
		Version:  "test",
		APIKey:   "test-key",
		Pool:     pl,
		Upstream: &upstream.Client{HTTP: hc, ChatHTTP: hc, ChatBaseCN: "https://fake.example", ChatBaseGlobal: "https://fake.example"},
	})
}

func probeDo(t *testing.T, p *Panel, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// waitProbeDone 轮询到探针结束（探针是异步的，前端也是轮询）。
func waitProbeDone(t *testing.T, p *Panel) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, out := probeDo(t, p, "GET", "/panel/api/probe/status", "")
		if running, _ := out["running"].(bool); !running {
			if done, _ := out["done"].(float64); done > 0 {
				return out
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("probe 未在 10s 内结束")
	return nil
}

// TestProbeDoesNotRunByItself 最关键的一条：**面板起来不会自己跑探针**。
//
// 用户明确要求"不要主动测试"。这条测试是那条约束的锚点——任何人加 ticker /
// 启动预热 / 额度刷新顺带探测，这里立刻红。
func TestProbeDoesNotRunByItself(t *testing.T) {
	fake := &probeRoundTripper{byModel: map[string]probeResp{}}
	p := newProbePanel(t, fake, &auth.Auth{UID: "gl-1", Domain: "www.workbuddy.ai"})

	// 给足时间让任何"启动预热/后台循环"有机会跑
	time.Sleep(150 * time.Millisecond)

	_, out := probeDo(t, p, "GET", "/panel/api/probe/status", "")
	if running, _ := out["running"].(bool); running {
		t.Error("面板创建后探针不应处于运行态")
	}
	if total, _ := out["total"].(float64); total != 0 {
		t.Errorf("未点过开始，total 应为 0，实际 %v", out["total"])
	}
	if rows, ok := out["rows"].([]any); ok && len(rows) != 0 {
		t.Errorf("未点过开始，不应有结果行，实际 %d 行", len(rows))
	}
	if cs := fake.callsSnapshot(); len(cs) != 0 {
		t.Errorf("未点过开始，不应有任何上游调用，实际 %d 次: %v", len(cs), cs)
	}
}

// TestProbeRealCallAndClassification 点了才跑：真实发请求 + 逐行分类。
func TestProbeRealCallAndClassification(t *testing.T) {
	fake := &probeRoundTripper{byModel: map[string]probeResp{
		"m-ok":      {200, "data: {\"choices\":[]}\n\n"},
		"m-credit":  {429, `{"code":14018,"msg":"Credits exhausted"}`},
		"m-rate":    {429, `{"code":14003,"msg":"too many requests"}`},
		"m-missing": {400, `{"code":11102,"msg":"service info not found"}`},
	}}
	p := newProbePanel(t, fake, &auth.Auth{UID: "cn-1"})

	rec, out := probeDo(t, p, "POST", "/panel/api/probe/start",
		`{"models":["m-ok","m-credit","m-rate","m-missing"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("start status=%d body=%s", rec.Code, rec.Body.String())
	}
	if total, _ := out["total"].(float64); total != 4 {
		t.Fatalf("total=%v want 4", out["total"])
	}

	st := waitProbeDone(t, p)
	rows, _ := st["rows"].([]any)
	if len(rows) != 4 {
		t.Fatalf("rows=%d want 4", len(rows))
	}
	got := map[string]map[string]any{}
	for _, r := range rows {
		m := r.(map[string]any)
		got[m["model"].(string)] = m
	}
	checks := []struct {
		model string
		ok    bool
		code  string
	}{
		{"m-ok", true, ""},
		{"m-credit", false, "14018"},
		{"m-rate", false, "14003"},
		{"m-missing", false, "11102"},
	}
	for _, c := range checks {
		m, exists := got[c.model]
		if !exists {
			t.Fatalf("缺 %s 的结果行", c.model)
		}
		if ok, _ := m["ok"].(bool); ok != c.ok {
			t.Errorf("%s ok=%v want %v", c.model, m["ok"], c.ok)
		}
		if code, _ := m["code"].(string); code != c.code {
			t.Errorf("%s code=%q want %q", c.model, code, c.code)
		}
		if m["note"] == "" {
			t.Errorf("%s 应有可读判定 note", c.model)
		}
	}
	// 真实调用：4 个模型都应打到上游（顺序不定，用集合比较）。
	// fake.calls 还会记下 14018 触发的余额刷新请求（该请求无 model 字段）⇒ 按空名过滤。
	var chatCalls []string
	for _, c := range fake.callsSnapshot() {
		if c != "" {
			chatCalls = append(chatCalls, c)
		}
	}
	if len(chatCalls) != 4 {
		t.Errorf("上游 chat 调用数=%d want 4（必须真实发出去）: %v", len(chatCalls), fake.callsSnapshot())
	}
	sum, _ := st["summary"].(map[string]any)
	if sum["ok"].(float64) != 1 || sum["hard_credit"].(float64) != 1 || sum["model_rate"].(float64) != 1 {
		t.Errorf("summary 分类不对: %v", sum)
	}
}

// TestProbeRealmPrefixFiltersAccounts global: 前缀的模型只应在 global 号上跑，
// 拿 global 模型打 cn 号是必然失败，测它只会污染结果。
func TestProbeRealmPrefixFiltersAccounts(t *testing.T) {
	fake := &probeRoundTripper{byModel: map[string]probeResp{}}
	p := newProbePanel(t, fake,
		&auth.Auth{UID: "cn-1"},
		&auth.Auth{UID: "gl-1", Domain: "www.workbuddy.ai"},
	)

	_, out := probeDo(t, p, "POST", "/panel/api/probe/start", `{"models":["global:hy4-preview-f"]}`)
	if total, _ := out["total"].(float64); total != 1 {
		t.Fatalf("total=%v want 1（只有 1 个 global 号）", out["total"])
	}
	st := waitProbeDone(t, p)
	rows, _ := st["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows=%d want 1", len(rows))
	}
	row := rows[0].(map[string]any)
	if row["uid"] != "gl-1" {
		t.Errorf("应只打到 global 号，实际 uid=%v", row["uid"])
	}
	if row["realm"] != "global" {
		t.Errorf("realm=%v want global", row["realm"])
	}
	// 上游收到的必须是**去前缀**的裸模型名
	if cs := fake.callsSnapshot(); len(cs) != 1 || cs[0] != "hy4-preview-f" {
		t.Errorf("上游应收到裸模型名 hy4-preview-f，实际 %v", cs)
	}
}

// TestProbeBareModelRunsOnBothRealms 裸模型名（无前缀）在两个 realm 都跑。
func TestProbeBareModelRunsOnBothRealms(t *testing.T) {
	fake := &probeRoundTripper{byModel: map[string]probeResp{}}
	p := newProbePanel(t, fake,
		&auth.Auth{UID: "cn-1"},
		&auth.Auth{UID: "gl-1", Domain: "www.workbuddy.ai"},
	)
	_, out := probeDo(t, p, "POST", "/panel/api/probe/start", `{"models":["hy3"]}`)
	if total, _ := out["total"].(float64); total != 2 {
		t.Fatalf("total=%v want 2", out["total"])
	}
	waitProbeDone(t, p)
	if cs := fake.callsSnapshot(); len(cs) != 2 {
		t.Errorf("上游调用=%d want 2", len(cs))
	}
}

// TestProbeUidFilter 只测指定账号（面板上勾号）。
func TestProbeUidFilter(t *testing.T) {
	fake := &probeRoundTripper{byModel: map[string]probeResp{}}
	p := newProbePanel(t, fake,
		&auth.Auth{UID: "cn-1"},
		&auth.Auth{UID: "cn-2"},
	)
	_, out := probeDo(t, p, "POST", "/panel/api/probe/start", `{"models":["hy3"],"uids":["cn-2"]}`)
	if total, _ := out["total"].(float64); total != 1 {
		t.Fatalf("total=%v want 1", out["total"])
	}
	st := waitProbeDone(t, p)
	rows, _ := st["rows"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["uid"] != "cn-2" {
		t.Fatalf("应只测 cn-2，实际 %v", rows)
	}
}

// TestProbeRejectsTooManyPairs 组合数超上限直接拒绝（防手滑勾全量打光限额）。
func TestProbeRejectsTooManyPairs(t *testing.T) {
	fake := &probeRoundTripper{byModel: map[string]probeResp{}}
	accts := make([]*auth.Auth, 0, 20)
	for i := 0; i < 20; i++ {
		accts = append(accts, &auth.Auth{UID: "cn-" + string(rune('a'+i))})
	}
	p := newProbePanel(t, fake, accts...)

	models := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		models = append(models, "m"+string(rune('a'+i)))
	}
	raw, _ := json.Marshal(map[string]any{"models": models})
	rec, _ := probeDo(t, p, "POST", "/panel/api/probe/start", string(raw))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("20x20=400 组合应被拒，实际 %d", rec.Code)
	}
	if cs := fake.callsSnapshot(); len(cs) != 0 {
		t.Errorf("被拒时不应发出任何上游调用，实际 %d", len(cs))
	}
}

// TestProbeEmptyModelsUsesRealmDefaults 空 models → 按账号所属 realm 取默认模型。
//
// 这是「国内版和国外版探针不一样」的锚点：同一个空请求，cn 号只探 cn 档、
// global 号探 global 三档。分流发生在服务端，前端不需要知道差异，也不会抄错。
func TestProbeEmptyModelsUsesRealmDefaults(t *testing.T) {
	// global 号 → global 默认档
	fake := &probeRoundTripper{byModel: map[string]probeResp{}}
	p := newProbePanel(t, fake, &auth.Auth{UID: "gl-1", Domain: "www.workbuddy.ai"})
	_, out := probeDo(t, p, "POST", "/panel/api/probe/start", `{}`)
	ms, _ := out["models"].([]any)
	if len(ms) != len(probeRealmModels["global"]) {
		t.Fatalf("global 号空 models 应用 %d 个默认模型，实际 %d：%v",
			len(probeRealmModels["global"]), len(ms), ms)
	}
	waitProbeDone(t, p)

	// cn 号 → 只探 cn 默认档
	fake2 := &probeRoundTripper{byModel: map[string]probeResp{}}
	p2 := newProbePanel(t, fake2, &auth.Auth{UID: "cn-1"})
	_, out2 := probeDo(t, p2, "POST", "/panel/api/probe/start", `{}`)
	ms2, _ := out2["models"].([]any)
	if len(ms2) != len(probeRealmModels["cn"]) {
		t.Fatalf("cn 号空 models 应用 %d 个默认模型，实际 %d：%v",
			len(probeRealmModels["cn"]), len(ms2), ms2)
	}
	waitProbeDone(t, p2)
}

// TestProbeRealmDefaultsAreDisjoint cn 与 global 的默认模型必须各归其域：
// 默认档里出现跨域模型（如 cn 默认里塞 global:xxx）会让探针必然打出假故障。
func TestProbeRealmDefaultsAreDisjoint(t *testing.T) {
	for realm, specs := range probeRealmModels {
		if len(specs) == 0 {
			t.Fatalf("realm %s 没有默认模型", realm)
		}
		for _, spec := range specs {
			pre, bare := splitProbeModel(spec)
			if pre != realm {
				t.Errorf("realm %s 的默认模型 %q 前缀是 %q，跨域", realm, spec, pre)
			}
			if bare == "" {
				t.Errorf("realm %s 的默认模型 %q 裸名为空", realm, spec)
			}
		}
	}
}

// TestProbeDefaultsAreAllFreeTier 默认探测档必须**全部是 0x 免费档**。
//
// 用户 2026-09-21 明确要求「hy4-preview 不要探测」—— 它在国际版是 x0.29 收费档，
// 探它是真花钱，而探针的定位是"零成本验证链路"（探针消耗上游额度、网关不记账，
// 每次点击都是净支出）。这条断言把"别把收费档加回默认列表"钉死在测试里：
// 以后有人觉得"hy4 系少了一个模型、补上吧"，CI 会直接拦下并指向这条要求。
func TestProbeDefaultsAreAllFreeTier(t *testing.T) {
	// 已知的收费档（国际版按 x0.29 计费）。探针默认档里出现任何一个都是回归。
	paid := map[string]string{
		"hy4-preview":   "国际版 x0.29 收费（国内版才是夜间免费）",
		"hy4-preview-x": "收费档",
	}
	for realm, specs := range probeRealmModels {
		for _, spec := range specs {
			_, bare := splitProbeModel(spec)
			if why, bad := paid[bare]; bad {
				t.Errorf("realm %s 的默认探测档含收费模型 %q（%s）—— 探针只用免费档，见 probeRealmModels 注释",
					realm, bare, why)
			}
		}
	}
	// 免费档的正面锚点：hy4 系只留 -f（0x 免费），别让整个 hy4 系消失。
	var hasFreeHy4 bool
	for _, spec := range probeRealmModels["global"] {
		if spec == "global:hy4-preview-f" {
			hasFreeHy4 = true
		}
	}
	if !hasFreeHy4 {
		t.Error("global 默认档应含 global:hy4-preview-f（0x 免费档，hy4 系的代表）")
	}
}

// TestProbeNoAutoRunAfterFinish 跑完就停，不会自动再跑一轮。
func TestProbeNoAutoRunAfterFinish(t *testing.T) {
	fake := &probeRoundTripper{byModel: map[string]probeResp{}}
	p := newProbePanel(t, fake, &auth.Auth{UID: "cn-1"})

	probeDo(t, p, "POST", "/panel/api/probe/start", `{"models":["hy3"]}`)
	waitProbeDone(t, p)
	n := len(fake.callsSnapshot())
	time.Sleep(200 * time.Millisecond) // 给"自动重跑"机会
	if now := len(fake.callsSnapshot()); now != n {
		t.Errorf("跑完后不应自动再跑：先前 %d 次，现在 %d 次", n, now)
	}
	_, out := probeDo(t, p, "GET", "/panel/api/probe/status", "")
	if running, _ := out["running"].(bool); running {
		t.Error("跑完 running 应为 false")
	}
}

// TestProbeAppliesResultsToPool 探针结果写回池状态。
//
// 用户 2026-09-21 要求「探针结果要计入账号自动切换器，额度不够的不再接入」。
// 三条路径各验一次：
//
//	14003 → 该号该模型进入模型级冷却（账号不连坐）
//	200   → 该号该模型冷却被清除（实测恢复比任何墙钟权威）
//	14018 → 账号级出池，**有效期 12h**；余额读数只做自校验，不写回
func TestProbeAppliesResultsToPool(t *testing.T) {
	const uid = "cn-1"
	fake := &probeRoundTripper{
		byModel: map[string]probeResp{
			"m-rate":   {429, `{"code":14003,"msg":"too many requests"}`},
			"m-credit": {429, `{"code":14018,"msg":"Credits exhausted"}`},
		},
		// 余额查询：套餐剩余 0 ⇒ P1（creditsKnown && credits<=0）应判该号不可选。
		// 注意信封是**小写** tag（apiEnvelope: code/msg/data），且 doJSON 返回的是
		// 信封里的 data 层 ⇒ 这里要比真实响应多包一层 data。
		nonChat: &probeResp{200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[` +
			`{"PackageName":"p","CapacitySize":100,"CapacityRemain":0,` +
			`"CycleCapacitySize":0,"CycleCapacityRemain":0}]}}}}`},
	}
	p := newProbePanel(t, fake, &auth.Auth{UID: uid})

	// 1) 14003 → 模型级冷却
	probeDo(t, p, "POST", "/panel/api/probe/start", `{"models":["m-rate"]}`)
	waitProbeDone(t, p)
	if mcs := p.cfg.Pool.ModelCooldowns(uid); mcs["m-rate"] == "" {
		t.Fatalf("14003 后应写入 m-rate 模型冷却，实际 %v", mcs)
	}

	// 2) 该模型实测恢复 → 冷却被清除
	//（BlockModelClear 只清 11102 前缀，清不掉 14003 ⇒ 必须走 ModelCooldownClear）
	fake.byModel["m-rate"] = probeResp{200, "data: {\"choices\":[]}\n\n"}
	probeDo(t, p, "POST", "/panel/api/probe/start", `{"models":["m-rate"]}`)
	waitProbeDone(t, p)
	if mcs := p.cfg.Pool.ModelCooldowns(uid); mcs["m-rate"] != "" {
		t.Fatalf("200 后应清除 m-rate 冷却，实际 %v", mcs)
	}

	// 3) 14018 → 账号级出池，**带 12h TTL**（用户要求：探测记录值保存 12 小时，
	//    之后默认恢复可以被切换器选择）。
	probeDo(t, p, "POST", "/panel/api/probe/start", `{"models":["m-credit"]}`)
	st := waitProbeDone(t, p)
	rows, _ := st["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows=%d want 1", len(rows))
	}
	row := rows[0].(map[string]any)
	t.Logf("applied=%q nonChats=%d available=%v",
		row["applied"], fake.nonChatCount(), p.cfg.Pool.AvailableUIDs())
	applied, _ := row["applied"].(string)
	if applied == "" {
		t.Errorf("14018 后应有写回动作描述，实际为空：%v", row)
	}
	if !strings.Contains(applied, "12h") {
		t.Errorf("写回描述必须写明有效期，实际 %q", applied)
	}
	if fake.nonChatCount() == 0 {
		t.Error("14018 应查一次余额（自校验：余额仍有则不该出池）")
	}
	for _, u := range p.cfg.Pool.AvailableUIDs() {
		if u == uid {
			t.Errorf("额度耗尽的号不应再纳入选号候选，实际仍在：%v", p.cfg.Pool.AvailableUIDs())
		}
	}
	reason, until, ok := p.cfg.Pool.ProbeVerdictOf(uid)
	if !ok {
		t.Fatalf("14018 应留下一条有效的探针结论，实际 %v", p.cfg.Pool.List())
	}
	if d := time.Until(until); d > 12*time.Hour || d < 12*time.Hour-time.Minute {
		t.Errorf("探针结论有效期 %v，want 约 12h", d)
	}
	if !strings.Contains(reason, "14018") {
		t.Errorf("结论原因应可追溯到 14018，实际 %q", reason)
	}
	// 探针**不写** credits：余额是 scheduler 每 5 分钟一轮的权威观测，探针再写一遍
	// 只会多出一个不会过期的第二判据，与「12h 后默认恢复」相矛盾。
	if cs, _ := p.cfg.Pool.Status(uid); cs.CreditsKnown {
		t.Errorf("探针不应写余额读数（会绕过 12h TTL），实际 %+v", cs)
	}
}

// TestProbeCreditVerdictSelfChecksBalance 14018 但余额仍有 ⇒ **不写回**。
// 探针的一手证据足够出池，但余额读数能证伪它（上游抖动/口径不符），
// 此时把好号钉死 12h 的代价远大于漏判一次。
func TestProbeCreditVerdictSelfChecksBalance(t *testing.T) {
	const uid = "cn-1"
	fake := &probeRoundTripper{
		byModel: map[string]probeResp{
			"m-credit": {429, `{"code":14018,"msg":"Credits exhausted"}`},
		},
		nonChat: &probeResp{200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[` +
			`{"PackageName":"p","CapacitySize":100,"CapacityRemain":77,` +
			`"CycleCapacitySize":0,"CycleCapacityRemain":0}]}}}}`},
	}
	p := newProbePanel(t, fake, &auth.Auth{UID: uid})

	probeDo(t, p, "POST", "/panel/api/probe/start", `{"models":["m-credit"]}`)
	st := waitProbeDone(t, p)
	rows, _ := st["rows"].([]any)
	applied, _ := rows[0].(map[string]any)["applied"].(string)
	if !strings.Contains(applied, "77") {
		t.Errorf("写回描述应报告真实余额，实际 %q", applied)
	}
	if _, _, ok := p.cfg.Pool.ProbeVerdictOf(uid); ok {
		t.Error("余额仍有 77 ⇒ 不应写探针结论")
	}
	found := false
	for _, u := range p.cfg.Pool.AvailableUIDs() {
		if u == uid {
			found = true
		}
	}
	if !found {
		t.Error("自校验不通过时应保留在池")
	}
}

// TestProbeDoesNotWriteBackOnAmbiguous 上游抖动（5xx / 传输错误）**不得**写回池状态：
// 那不是"账号不可用"的证据，误写会把好号踢出轮换。
func TestProbeDoesNotWriteBackOnAmbiguous(t *testing.T) {
	const uid = "cn-1"
	fake := &probeRoundTripper{byModel: map[string]probeResp{
		"m-500": {500, `{"msg":"internal error"}`},
	}}
	p := newProbePanel(t, fake, &auth.Auth{UID: uid})

	probeDo(t, p, "POST", "/panel/api/probe/start", `{"models":["m-500"]}`)
	st := waitProbeDone(t, p)
	rows, _ := st["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows=%d want 1", len(rows))
	}
	if applied, _ := rows[0].(map[string]any)["applied"].(string); applied != "" {
		t.Errorf("5xx 不应写回池状态，实际写了：%q", applied)
	}
	if mcs := p.cfg.Pool.ModelCooldowns(uid); len(mcs) != 0 {
		t.Errorf("5xx 不应产生模型冷却，实际 %v", mcs)
	}
	if n := fake.nonChatCount(); n != 0 {
		t.Errorf("5xx 不应触发余额刷新，实际 %d 次", n)
	}
}

// TestSplitProbeModel 前缀解析：只认 cn/global，其他前缀当模型名一部分。
func TestSplitProbeModel(t *testing.T) {
	cases := []struct{ in, realm, bare string }{
		{"cn:hy3", "cn", "hy3"},
		{"global:hy4-preview-f", "global", "hy4-preview-f"},
		{"hy3", "", "hy3"},
		{"gpt-5.6-sol", "", "gpt-5.6-sol"},
		{"x:y", "", "x:y"},
	}
	for _, c := range cases {
		r, b := splitProbeModel(c.in)
		if r != c.realm || b != c.bare {
			t.Errorf("splitProbeModel(%q)=(%q,%q) want (%q,%q)", c.in, r, b, c.realm, c.bare)
		}
	}
}

// TestProbeRunsSeriallyWithInterval 探针必须**串行**：并发恒为 1，且相邻探测之间有间隔。
//
// 用户 2026-09-22：「探针一个个账号测试，设计间隔，不要并发」—— 并发会把免费档打成
// 14003，测到的是探针自己的副作用而不是账号可用性。这两条断言是防回归的：改回并发会立刻红。
func TestProbeRunsSeriallyWithInterval(t *testing.T) {
	old := probeInterval
	probeInterval = 120 * time.Millisecond
	defer func() { probeInterval = old }()

	// delay > 0 是必须的：请求若瞬时返回，即便真的并发也会观测到峰值 1 ⇒ 断言空转。
	fake := &probeRoundTripper{delay: 60 * time.Millisecond}
	p := newProbePanel(t, fake,
		&auth.Auth{UID: "gl-1", Domain: "www.workbuddy.ai"},
		&auth.Auth{UID: "gl-2", Domain: "www.workbuddy.ai"},
		&auth.Auth{UID: "gl-3", Domain: "www.workbuddy.ai"},
		&auth.Auth{UID: "gl-4", Domain: "www.workbuddy.ai"},
	)

	start := time.Now()
	probeDo(t, p, "POST", "/panel/api/probe/start", `{"models":["m-one"]}`)
	st := waitProbeDone(t, p)
	elapsed := time.Since(start)

	if rows, _ := st["rows"].([]any); len(rows) != 4 {
		t.Fatalf("rows=%d want 4（4 个账号 × 1 个模型）", len(rows))
	}
	if got := fake.maxConcurrent(); got != 1 {
		t.Errorf("并发峰值=%d want 1 —— 探针必须串行；并发会把免费档打成 14003，"+
			"测到的就是探针的副作用而非账号可用性", got)
	}
	// 4 次探测之间有 3 个间隔。下界断言：间隔没生效就会连发，撞免费档限额。
	if want := 3 * probeInterval; elapsed < want {
		t.Errorf("耗时 %v < 3×间隔 %v —— 间隔没生效（连发会撞免费档 per-model 限额）", elapsed, want)
	}
}

// TestProbeStatusReportsSerialContract 状态接口必须把「串行 + 间隔」报给前端。
// 前端靠 interval_ms 估时；若重新引入并发而不同步这个契约，面板会把耗时低估成 1/3。
func TestProbeStatusReportsSerialContract(t *testing.T) {
	fake := &probeRoundTripper{}
	p := newProbePanel(t, fake, &auth.Auth{UID: "gl-1", Domain: "www.workbuddy.ai"})
	_, out := probeDo(t, p, "GET", "/panel/api/probe/status", "")

	if got, _ := out["max_concurrency"].(float64); got != 1 {
		t.Errorf("max_concurrency=%v want 1（探针串行；前端按此估时）", out["max_concurrency"])
	}
	if got, _ := out["interval_ms"].(float64); got <= 0 {
		t.Errorf("interval_ms=%v 应 > 0（前端用它算预计耗时）", out["interval_ms"])
	}
}
