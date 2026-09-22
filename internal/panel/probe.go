package panel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// probe.go 手动探针：**点了才跑**的真实可用性探测。
//
// 设计边界（用户 2026-09-21 明确要求，改动前务必先读）：
//
//  1. **绝不自动跑**。没有 ticker、没有后台循环、不在启动时预热、不随额度刷新顺带跑。
//     唯一触发点是 POST /panel/api/probe/start。想加"定期探测"请先问用户——他明确否过。
//  2. **真实调用**。对每个 (账号, 模型) 组合用**该账号自己的 auth 直连上游**发一次最小
//     流式请求（max_tokens=16）。不是模拟、不读缓存、不复用上次结果。
//     直连（而非走网关）是必须的：走网关会被选号器自动换号，测不出"哪个号不可用"。
//  3. **结果写回池状态**（2026-09-21 用户要求「探针结果要计入账号自动切换器，额度不够的
//     不再接入」）。写回规则见 applyProbeResult：只对语义明确的结果动手，上游抖动一律不写回。
//     **账号级结论带 12h TTL**（用户补充：「探测记录值保存 12 小时，之后默认恢复可以被
//     切换器选择」）——探针是一次快照而非持续观测，没有 TTL 就会把好号永久钉在池外。
//     代价：探针消耗的上游限额网关不记账，所以**必须串行执行（并发恒为 1）+ 固定间隔 +
//     总量上限**（见下方常量），否则探针自己会把免费档打到限流，反而制造假故障。
//  4. **互斥**。同一时刻只允许一个探针在跑，重复点击返回 409，避免并发打爆上游。

const (
	// probeMaxPairs 单次探针的组合数上限（账号 × 模型）。防手滑勾成全量（7×52=364）
	// 把上游限额打光。超限直接拒绝，不截断（静默截断会让人误以为"全测过了"）。
	probeMaxPairs = 120

	// probeMaxTokens 探测请求的输出上限。只验证"能不能调通"，越小越省额度。
	probeMaxTokens = 16

	// probeDrainLimit 200 响应最多排空多少字节。max_tokens=16 的流很小，
	// 这里只是防御上游异常长流把内存吃满。
	probeDrainLimit = 256 << 10
)

// probeInterval 相邻两次探测之间的固定间隔。**探针全程串行，并发恒为 1。**
//
// 用户 2026-09-22 要求「探针一个个账号测试，设计间隔，不要并发」。理由与原先"压并发"一样，
// 只是更彻底：免费档（尤其 hy4 系）的 per-model 限额很紧，连发会把探针自己打成 14003 ——
// 那测到的是**探针的副作用**，不是账号的真实可用性。串行 + 间隔后每个结论彼此独立，
// 代价是单次全量（120 组合）约 3 分钟；探针本就是人工点击的低频运维动作，这个代价值得。
//
// 声明为 var 而非 const：单测要把它压到毫秒级才能在合理时间内验证"确实串行"。
// **生产代码不得改写它。**
var probeInterval = 1500 * time.Millisecond

// probeRealmModels 各 realm 的默认探测模型（用户 2026-09-21 指定「国内版和国外版探针不一样」）：
//   - 国内版：只探 deepseek-v4.1-flash
//   - 国际版：deepseek-v4.1-flash + hy4-preview-f
//
// 为什么按 realm 分：cn 号打 global 模型必然失败（realm 隔离），测它没有信息量。
// 为什么选这几个：**全部是 0x 免费档**，探测成本为零；且足以覆盖"额度是否耗尽"这一核心判据
// ——额度耗尽的号打**任何**模型都回 14018，用免费档验最省。
//
// 🔴 hy4-preview 刻意**不在**探测列表里（用户 2026-09-21 明确要求「hy4-preview 不要探测」）。
// 它与 hy4-preview-f 是**两个独立模型、各自独立限流**，但定价完全不同：
//   - hy4-preview-f  国际版专属，促销 hy4-f-free-trial-202608（9/11–10/10）**全天 0x 免费**
//   - hy4-preview     国际版**无促销** ⇒ 按 **x0.29 计费**（国内版才是夜间免费 23:00–8:00）
//
// ⇒ 探它是**真花钱**的，而探针的定位是"零成本验证链路"，所以只用 -f 代表 hy4 系。
// 别把这条当漏配补回去：探针只要拿到 14018 就足以判定账号额度耗尽，多探一个收费档
// 既不加信息量，又会在每次点击时白扣额度。
//
// 判据来源是官方客户端产品配置 modelPromotions（不是网关目录 credits，后者读不到促销价）。
// 详见 wb2api-models/FREE-MODELS.md、PROBE.md。
var probeRealmModels = map[string][]string{
	"cn":     {"cn:deepseek-v4.1-flash"},
	"global": {"global:deepseek-v4.1-flash", "global:hy4-preview-f"},
}

// probeModelCooldown 探针判定「模型级限流（14003）」时写入的模型冷却时长。
//
// 与 handler.softCooldown 的默认值保持一致（600s）。panel 侧拿不到 live 配置，
// 故用同一默认值 —— 宁可两处都取默认，也不要让探针与真实请求路径给出两套冷却语义。
const probeModelCooldown = 600 * time.Second

// reProbeCode 从上游响应体提取业务 code（14018 / 14003 / 11102 …）。
// 纯展示用途：面板只把它显示给人看，**不据此做任何路由或池状态决策**。
var reProbeCode = regexp.MustCompile(`"(?:code|Code)"\s*:\s*"?(\d+)"?`)

// probeRow 一次 (账号, 模型) 探测的结果。
type probeRow struct {
	UID    string `json:"uid"`
	Realm  string `json:"realm"`
	Model  string `json:"model"`
	Status int    `json:"status"` // HTTP 状态；-1 = 传输层错误（连不上/DNS/超时）
	OK     bool   `json:"ok"`
	Code   string `json:"code,omitempty"` // 上游业务 code 原文（如 "14018"）
	Kind   string `json:"kind,omitempty"` // 上游分类（hard_credit / soft_rate / model_blocked …）
	MS     int64  `json:"ms"`             // 耗时（毫秒）
	Err    string `json:"err,omitempty"`  // 传输层错误摘要
	Note   string `json:"note,omitempty"` // 人类可读判定（如"额度耗尽"）
	// Applied 本次结果对池状态的**实际写回动作**（空 = 未写回）。
	// 单独一列是为了让"探测到了"与"已影响选号"可区分——两者不是一回事。
	Applied string `json:"applied,omitempty"`
}

// probePair 一次待探测的 (账号, 模型) 组合。
// 必须包级命名（不能用函数内局部类型）：runProbe 的签名要用到它。
type probePair struct {
	uid   string
	realm string
	model string // 展示名（带 realm: 前缀）
	bare  string // 上游实际模型名（去前缀）
}

// probeJob 手动探针的单例状态（面板进程内常驻）。
type probeJob struct {
	mu        sync.Mutex
	running   bool
	cancel    context.CancelFunc
	total     int
	done      int
	models    []string
	rows      []probeRow
	startedAt time.Time
	endedAt   time.Time
	note      string
}

// probeSummary 结果汇总（面板顶部一行）。
type probeSummary struct {
	OK         int `json:"ok"`
	HardCredit int `json:"hard_credit"` // 14018 账号额度耗尽
	ModelRate  int `json:"model_rate"`  // 14003 模型级限流
	Other      int `json:"other"`
	Total      int `json:"total"`
}

// probeSnapshot 返回给前端的快照（轮询用）。
type probeSnapshot struct {
	Running   bool         `json:"running"`
	Total     int          `json:"total"`
	Done      int          `json:"done"`
	StartedAt string       `json:"started_at,omitempty"`
	EndedAt   string       `json:"ended_at,omitempty"`
	ElapsedMS int64        `json:"elapsed_ms"`
	Models    []string     `json:"models"`
	Rows      []probeRow   `json:"rows"`
	Summary   probeSummary `json:"summary"`
	Note      string       `json:"note,omitempty"`
	// DefaultModels 各 realm 的默认探测模型：请求体不传 models 时，按每个账号**所属
	// realm** 自动取（国内版只探 cn 档，国际版探 global 两档）。前端从这里取，
	// **不在 JS 里再抄一份**——抄两份就会漂移（今天刚在 handler/panel 的 catalog 上栽过）。
	DefaultModels map[string][]string `json:"default_models"`
	// MaxPairs / IntervalMS / MaxConcurrency 供前端做"将发起 N 次调用"的预估与提示。
	// 探针**串行**执行（用户 2026-09-22 要求不并发），所以 MaxConcurrency 恒为 1，
	// 耗时由 IntervalMS 决定 —— 前端必须按"串行 + 间隔"估时，别再按并发数除。
	MaxPairs       int `json:"max_pairs"`
	IntervalMS     int `json:"interval_ms"`
	MaxConcurrency int `json:"max_concurrency"`
	// VerdictTTLSeconds 探针账号级结论的有效期（12h）。前端只展示，不参与计算 ——
	// 真正的到期判定在 pool 侧（until 字段），这里透出是为了让运维知道
	// "这条出池记录会自己消失"。
	VerdictTTLSeconds int `json:"verdict_ttl_seconds"`
}

// probeStart 启动一次探针。请求体（全部可选）：
//
//	{"models": ["global:hy4-preview-f", ...],   // 空 = 免费档预填
//	 "uids":   ["<uid>", ...]}                  // 空 = 全部账号
//
// 模型名支持 `realm:model` 前缀（只在该 realm 的账号上跑）或裸名（两个 realm 都跑）。
func (p *Panel) probeStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Models []string `json:"models"`
		Uids   []string `json:"uids"`
	}
	// body 可空：空 body 用默认值。限制 64KB 防异常请求。
	_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req)

	// models 可空 ⇒ 按每个账号**所属 realm** 取默认（国内版只探 cn 档，国际版探 global 两档）。
	// 这正是"国内版和国外版探针不一样"的落点：同一份配置，按号分域自动分流。
	// ⚠️ 原注释误写"global 三档"（与 probeRealmModels 实际两档不符，2026-09-22 修正）。
	// `hy3` 虽也是 0x 免费档，但用户 2026-09-22 明确「这个模型我基本不用」⇒ 不进默认档；
	// 需要时在面板显式填 `global:hy3` / `cn:hy3` 仍可探。
	models := normalizeProbeModels(req.Models)
	if p.cfg.Pool == nil {
		writeErr(w, http.StatusServiceUnavailable, "no pool")
		return
	}

	uidFilter := map[string]bool{}
	for _, u := range req.Uids {
		if u = strings.TrimSpace(u); u != "" {
			uidFilter[u] = true
		}
	}

	// 展开 (账号, 模型) 组合。realm 前缀决定该模型只在同 realm 账号上跑——
	// 拿 global 模型打 cn 号是必然失败，测它没有信息量，只会污染结果。
	var pairs []probePair
	used := map[string]bool{}
	for _, st := range p.cfg.Pool.List() {
		if len(uidFilter) > 0 && !uidFilter[st.UID] {
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		realm := a.Realm()
		specs := models
		if len(specs) == 0 {
			specs = probeRealmModels[realm]
		}
		for _, spec := range specs {
			pre, bare := splitProbeModel(spec)
			if pre != "" && pre != realm {
				continue
			}
			pairs = append(pairs, probePair{uid: st.UID, realm: realm, model: spec, bare: bare})
			used[spec] = true
		}
	}
	if len(pairs) == 0 {
		writeErr(w, http.StatusBadRequest,
			"no (account, model) pair matched; check realm prefixes / account selection")
		return
	}
	if len(pairs) > probeMaxPairs {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("too many combinations: %d > %d; narrow the selection", len(pairs), probeMaxPairs))
		return
	}

	// 实际用到的模型列表（去重 + 排序，结果稳定便于前端与测试断言）。
	modelList := make([]string, 0, len(used))
	for m := range used {
		modelList = append(modelList, m)
	}
	sort.Strings(modelList)

	j := p.probeJob()
	j.mu.Lock()
	if j.running {
		j.mu.Unlock()
		writeErr(w, http.StatusConflict, "probe already running")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	j.running = true
	j.cancel = cancel
	j.total = len(pairs)
	j.done = 0
	j.rows = make([]probeRow, 0, len(pairs))
	j.models = modelList
	j.startedAt = time.Now()
	j.endedAt = time.Time{}
	j.note = ""
	j.mu.Unlock()

	go p.runProbe(ctx, pairs)
	// 回 modelList（实际展开的），不是 req.Models —— 空请求时后者为空，
	// 前端会显示"探测了 0 个模型"而实际跑了 N 个。
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "total": len(pairs), "models": modelList})
}

// probeStatus 返回当前探针状态（前端轮询；从未跑过 → running=false + 空行集）。
func (p *Panel) probeStatus(w http.ResponseWriter, r *http.Request) {
	j := p.probeJob()
	j.mu.Lock()
	snap := probeSnapshot{
		Running: j.running,
		Total:   j.total,
		Done:    j.done,
		Models:  append([]string(nil), j.models...),
		Rows:    append([]probeRow(nil), j.rows...),
		Note:    j.note,
		// 只读常量，直接引用即可（无人会改写 probeRealmModels）。
		DefaultModels:     probeRealmModels,
		MaxPairs:          probeMaxPairs,
		IntervalMS:        int(probeInterval / time.Millisecond),
		MaxConcurrency:    1, // 串行：并发恒为 1（见 probeInterval 注释）
		VerdictTTLSeconds: int(pool.ProbeVerdictTTL / time.Second),
	}
	if !j.startedAt.IsZero() {
		snap.StartedAt = j.startedAt.Format(time.RFC3339)
	}
	if !j.endedAt.IsZero() {
		snap.EndedAt = j.endedAt.Format(time.RFC3339)
	}
	end := time.Now()
	if !j.endedAt.IsZero() {
		end = j.endedAt
	}
	if !j.startedAt.IsZero() {
		snap.ElapsedMS = end.Sub(j.startedAt).Milliseconds()
	}
	j.mu.Unlock()

	if snap.Rows == nil {
		snap.Rows = []probeRow{}
	}
	for _, row := range snap.Rows {
		snap.Summary.Total++
		switch {
		case row.OK:
			snap.Summary.OK++
		case row.Code == "14018":
			snap.Summary.HardCredit++
		case row.Code == "14003":
			snap.Summary.ModelRate++
		default:
			snap.Summary.Other++
		}
	}
	writeJSON(w, http.StatusOK, snap)
}

// probeCancel 取消进行中的探针（已完成的行走结果保留）。
func (p *Panel) probeCancel(w http.ResponseWriter, r *http.Request) {
	j := p.probeJob()
	j.mu.Lock()
	if !j.running || j.cancel == nil {
		j.mu.Unlock()
		writeErr(w, http.StatusConflict, "no probe running")
		return
	}
	cancel := j.cancel
	j.mu.Unlock()
	cancel()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// runProbe 执行探针：有界并发跑完全部组合，逐行追加结果（前端轮询即"实时更新"）。
func (p *Panel) runProbe(ctx context.Context, pairs []probePair) {
	j := p.probeJob()
	defer func() {
		j.mu.Lock()
		j.running = false
		j.cancel = nil
		j.endedAt = time.Now()
		if ctx.Err() != nil {
			j.note = "已取消（结果保留已完成部分）"
		}
		j.mu.Unlock()
	}()

	// 🔴 串行执行：**并发恒为 1**（用户 2026-09-22 明确要求「一个个账号测试，不要并发」）。
	// 并发会让免费档被探针自己打成 14003 —— 那测到的是探针的副作用，不是账号的真实可用性。
	// 每次探测之间睡 probeInterval；取消时不等满、立刻退出。
	for i, pr := range pairs {
		if ctx.Err() != nil {
			break
		}
		acct := p.cfg.Pool.AuthByUID(pr.uid)
		if acct == nil {
			continue
		}
		if i > 0 {
			select {
			case <-time.After(probeInterval):
			case <-ctx.Done():
			}
			if ctx.Err() != nil {
				break
			}
		}
		row := p.probeOne(ctx, pr.uid, pr.realm, pr.model, pr.bare)
		j.mu.Lock()
		j.rows = append(j.rows, row)
		j.done++
		j.mu.Unlock()
	}
}

// probeOne 对单个 (账号, 模型) 发一次真实最小请求并判定。
//
// 用该账号自己的 auth 直连上游：绕过网关的选号器与池记账，才能测出"这个号这个模型
// 现在到底行不行"。**探测本身**不写池状态（直连、不经过选号器）；写回是它的下游一步，
// 由 applyProbeResult 按语义明确的三类结果单独执行（见文件头第 3 条）。
func (p *Panel) probeOne(ctx context.Context, uid, realm, model, bare string) probeRow {
	row := probeRow{UID: uid, Realm: realm, Model: model}
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		row.Status = -1
		row.Err = "account gone"
		return row
	}

	body := []byte(fmt.Sprintf(
		`{"model":%q,"max_tokens":%d,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		bare, probeMaxTokens))

	// ConversationRequestID 是上游必发头（X-Conversation-Request-ID），每次新生成：
	// 复用同一个 id 会让上游把多次探测当成同一次会话的重复请求。
	meta := upstream.ChatMeta{ConversationRequestID: probeRandomID()}

	start := time.Now()
	rc, status, respBody, err := p.cfg.Upstream.ChatStreamContext(ctx, a, body, "", meta)
	row.MS = time.Since(start).Milliseconds()

	if rc != nil {
		// 200 时排空流（让上游把这次请求走完，避免半途断连留下脏状态）；
		// 非 200 时 rc 一般为 nil，防御性处理。
		_, _ = io.Copy(io.Discard, io.LimitReader(rc, probeDrainLimit))
		_ = rc.Close()
	}

	var uerr *upstream.Error
	if errors.As(err, &uerr) {
		status = uerr.Status
	}
	row.Status = status

	if err != nil && status < 400 {
		// 传输层错误（连不上/超时/DNS）：status 可能为 0，统一记 -1 便于前端区分。
		row.Status = -1
		row.Err = truncateRunes(err.Error(), 160)
		return row
	}
	if status == http.StatusOK {
		row.OK = true
		row.Applied = p.applyProbeResult(uid, bare, row)
		return row
	}

	// 非 200：上游原文优先（respBody 是原始 body；err 里也可能带同一份）。
	raw := string(respBody)
	if raw == "" && uerr != nil {
		raw = uerr.Msg
	}
	if m := reProbeCode.FindStringSubmatch(raw); m != nil {
		row.Code = m[1]
	}
	row.Kind = upstream.Classify(status, raw).String()
	row.Note = probeNote(row.Code, row.Kind, status)
	row.Applied = p.applyProbeResult(uid, bare, row)
	return row
}

// applyProbeResult 把一次探测结果写回池状态，返回写回动作的人话描述（空 = 未写回）。
//
// 用户 2026-09-21 要求「探针结果要计入账号自动切换器，额度不够的不再接入」，
// 并补充「探测记录值保存 12 小时，之后默认恢复可以被切换器选择」。
// 但"探测到了"≠"该写回"：上游抖动（5xx/网络/参数错）不代表账号不可用，写回会误伤好号。
// 所以只对**语义明确**的三类结果动手：
//
//	200         → NoteSuccess + ModelCooldownClear + ProbeCooldownClear：
//	              该号该模型实测通，解除模型级避让，并撤销 12h 前的额度耗尽结论
//	              （14018 是账号级判据，任一模型实测通即足以证伪整条结论）
//	429 + 14003 → CooldownModelRateLimit：模型级冷却，**账号不连坐**（与 handler F1 同语义）
//	429 + 14018 → CooldownProbe：账号级出池，有效期固定 ProbeVerdictTTL（12h）
//	其它        → 不写回
//
// 12h TTL 是**结构性**保证，不靠定时任务：CoolProbe 走的还是 until 字段，到期
// healthy() 自然放行 ⇒ 账号回到默认候选集，由真实流量重新判定（判错只浪费一轮上游调用，
// 现有错误路径会立刻重新出池）。提前恢复的路径还有三条：实测 200、余额刷新回正
// （scheduler 每 5 分钟一轮的 ReenableIfCredits 会清掉它）、人工解冻。
//
// 14018 为什么要先做一次**只读**余额查询：探针的一手证据（真实调用回了 14018）足够
// 出池，但余额读数能给运维一个具体数字，更重要的是**自校验** —— 若查询回来仍有余额，
// 说明 14018 另有原因，此时不写结论、保留在池，避免一次误判把好号钉死 12h。
// 注意这里**不写** credits：余额是 scheduler 每 5 分钟一轮的权威观测（ReenableIfCredits），
// 探针再写一遍只会多出一个不会过期的第二判据，与「12h 后默认恢复」相矛盾。
func (p *Panel) applyProbeResult(uid, bare string, row probeRow) string {
	if p.cfg.Pool == nil {
		return ""
	}
	if row.OK {
		p.cfg.Pool.NoteSuccess(uid)
		recovered := p.cfg.Pool.ProbeCooldownClear(uid)
		cleared := p.cfg.Pool.ModelCooldownClear(uid, bare)
		switch {
		case recovered && cleared:
			return "已解除该模型避让，并撤销额度耗尽结论（账号回池）"
		case recovered:
			return "已撤销额度耗尽结论（账号回池）"
		case cleared:
			return "已解除该模型避让"
		}
		return ""
	}
	switch row.Code {
	case "14003":
		p.cfg.Pool.CooldownModelRateLimit(uid, probeModelCooldown, bare, "14003 probe model rate limit")
		return "该模型已冷却 " + probeModelCooldown.String() + "（账号不受影响）"
	case "14018":
		// 只读余额查询：既给运维一个具体数字，又做**自校验**（余额仍有 ⇒ 14018 另有原因，
		// 不写结论）。查询失败不阻断——一手 14018 证据本身已足够出池。
		note := ""
		if p.cfg.Upstream != nil {
			if a := p.cfg.Pool.AuthByUID(uid); a != nil {
				remain, _, err := p.cfg.Upstream.UserResource(a)
				if err == nil && remain > 0 {
					return fmt.Sprintf("余额仍有 %d ⇒ 未写回（14018 非额度原因，保留在池）", remain)
				}
				if err != nil {
					note = "余额查询失败，按实测结论"
				} else {
					note = "余额读数为 0"
				}
			}
		}
		if !p.cfg.Pool.CooldownProbe(uid, "14018 probe credit exhausted") {
			return "账号已不在池中，未写回"
		}
		if note == "" {
			return "已出池，12h 后自动回到轮换"
		}
		return "已出池，12h 后自动回到轮换（" + note + "）"
	}
	return ""
}

// probeNote 把 (code, kind, status) 翻成一句人话。纯展示，不参与任何决策。
func probeNote(code, kind string, status int) string {
	switch code {
	case "14018":
		return "账号额度耗尽（连免费档也不豁免）"
	case "14003":
		return "该模型在此号被限流（模型级，同号其它模型不受影响）"
	case "11102":
		return "该后端无此模型"
	}
	switch kind {
	case "hard_credit":
		return "额度不足"
	case "soft_rate":
		return "上游限流"
	case "session_dead":
		return "会话失效（需重新登录）"
	case "model_blocked":
		return "模型不存在"
	case "waf_block":
		return "出口 IP 被 WAF 拦截"
	case "server":
		return "上游 5xx"
	}
	return fmt.Sprintf("HTTP %d", status)
}

// probeJob 返回探针单例（懒建）。
func (p *Panel) probeJob() *probeJob {
	p.probeOnce.Do(func() { p.probeState = &probeJob{} })
	return p.probeState
}

// normalizeProbeModels 去空白、去重、保持输入顺序；空输入 → 免费档预填。
func normalizeProbeModels(in []string) []string {
	if len(in) == 0 {
		// 空 ⇒ 返回 nil，由 probeStart 按每个账号的 realm 取 probeRealmModels。
		// 不在这里展开：本函数看不到账号，展开会丢掉"按域分流"这个语义。
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// splitProbeModel 拆 `realm:model` 前缀。无前缀 → ("", 原名)。
// 只认 cn/global 两个前缀（与网关路由协议同集合）；其他前缀当模型名的一部分。
func splitProbeModel(spec string) (realm, bare string) {
	for _, r := range []string{"cn", "global"} {
		if strings.HasPrefix(spec, r+":") {
			return r, strings.TrimPrefix(spec, r+":")
		}
	}
	return "", spec
}

// probeRandomID 生成 36 位十六进制请求 id（与上游 X-Conversation-Request-ID 同形态）。
func probeRandomID() string {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
