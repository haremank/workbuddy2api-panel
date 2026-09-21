package panel

import (
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestAppJSSyntax app.js 必须能通过 JS 解析器语法校验。
//
// 为什么需要：app.js 是 go:embed 进二进制的静态资源，Go 编译器不检查其内容——
// 一次对象字面量键名未加引号（Model_chat_GLM5.2 被解析成属性访问 + 数字字面量）
// 就让整个面板白屏，而所有 Go 测试依然全绿。此测试把语法校验前移到 CI。
// 无 node 环境时跳过（不阻塞无 Node 的构建机）。
func TestAppJSSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JS syntax check")
	}
	path, err := filepath.Abs("app.js")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "--check", path).CombinedOutput()
	if err != nil {
		t.Fatalf("app.js syntax error:\n%s", out)
	}
}

// TestIndexHTMLNoInlineScript index.html 不得含内联 <script> 块：
// 严格 CSP（script-src 'self'）会拦截内联脚本，页面将完全不可用。
// 外链形式 <script src="..."> 允许。
func TestIndexHTMLNoInlineScript(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	rest := body
	for {
		idx := strings.Index(rest, "<script")
		if idx < 0 {
			break
		}
		rest = rest[idx:]
		end := strings.Index(rest, ">")
		if end < 0 {
			break
		}
		tag := rest[:end+1]
		if !strings.Contains(tag, "src=") {
			t.Fatalf("index.html contains inline <script> (blocked by CSP): %s", tag)
		}
		rest = rest[end:]
	}
}

// reHTMLID 匹配 index.html 里的 id 属性（静态资源，双引号形式）。
var reHTMLID = regexp.MustCompile(`\bid="([^"]+)"`)

// reJSGetElem 匹配 app.js 里的 $('xxx') 取元素调用（本仓约定：单引号 + 字面量 id）。
var reJSGetElem = regexp.MustCompile(`\$\(\s*'([^']+)'\s*\)`)

// TestAppJSRefsExistInHTML 锚定 app.js 里每个 $('id') 引用的元素在 index.html 里真实存在。
//
// 为什么需要：app.js 与 index.html 都是 go:embed 的静态资源，Go 编译器不检查其内容，
// 而 $('x') 取不到元素只返回 null —— 后续 .onclick / .value 会抛 TypeError 静默失效
// （按钮点了没反应、页面局部白屏），所有 Go 测试却依然全绿。本轮新增手动探针 UI
// （7 个 pb*/btnProbe* 元素）正踩在这条缝上，故把「引用 ↔ 元素」一致性前移到 CI。
// 反向（HTML 有 id 但 JS 没引用）不检查：模板里存在仅供 CSS 或语义用途的 id 是正常的。
func TestAppJSRefsExistInHTML(t *testing.T) {
	ids := map[string]bool{}
	for _, m := range reHTMLID.FindAllStringSubmatch(string(indexHTML), -1) {
		ids[m[1]] = true
	}
	if len(ids) == 0 {
		t.Fatal("no id= found in index.html; regex or asset changed")
	}

	var missing []string
	seen := map[string]bool{}
	for _, m := range reJSGetElem.FindAllStringSubmatch(string(appJS), -1) {
		id := m[1]
		if seen[id] {
			continue
		}
		seen[id] = true
		if !ids[id] {
			missing = append(missing, id)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no $('...') found in app.js; regex or asset changed")
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("app.js references elements missing from index.html "+
			"($() would return null and silently break):\n  %s",
			strings.Join(missing, "\n  "))
	}
}

// TestProbeUIElementsPresent 手动探针的关键元素必须同时存在于 HTML 与 JS 引用中。
//
// 这是本轮交付（手动探针）的最小契约锚点：一旦有人改名/删元素却漏改另一侧，
// 上面的通用测试会兜住，但这条能给出"是哪块功能坏了"的直接线索。
func TestProbeUIElementsPresent(t *testing.T) {
	// 后端 probeStatus 返回的字段名与前端消费处必须一致；此处锚定前端元素。
	want := []string{"pbModels", "pbAccts", "pbPlan", "pbNote", "pbBody", "btnProbeRun", "btnProbeCancel"}

	html := string(indexHTML)
	js := string(appJS)
	for _, id := range want {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html missing probe element id=%q", id)
		}
		if !strings.Contains(js, `$('`+id+`')`) {
			t.Errorf("app.js missing probe reference $('%s')", id)
		}
	}
}

// TestProbeWritebackUIContract 探针「写回选号器」这一列的契约（2026-09-21 新增）：
// 后端 probeRow.Applied → 表头一列 → 前端渲染，三处必须同时在场。
//
// 另外锚定两件刚踩过的事：
//   - 默认探测档按 **realm** 分流后，后端字段是 default_models，前端**不得**再引用
//     已删除的 free_tier（引用了就是静默失效：不报错、只是永远不生效）。
//   - 表头列数必须与 pbRow 实际渲染的 <td> 数一致，否则表头与数据错位一格
//     （表头 7 列、数据 8 格这种错误肉眼很难在宽表里看出来）。
func TestProbeWritebackUIContract(t *testing.T) {
	html := string(indexHTML)
	js := string(appJS)

	if !strings.Contains(html, "写回选号器") {
		t.Error("index.html 探针表缺少「写回选号器」列")
	}
	if !strings.Contains(js, "r.applied") {
		t.Error("app.js 未渲染 probeRow.applied（写回列会是空的）")
	}
	if !strings.Contains(js, "default_models") {
		t.Error("app.js 未消费 default_models（按域分流会失效）")
	}
	if strings.Contains(js, "free_tier") {
		t.Error("app.js 仍在引用已删除的 free_tier 字段（静默失效）")
	}
	if !strings.Contains(js, "probe_verdict") {
		t.Error("app.js 未识别 cool_kind=probe_verdict（探针判定的号会被显示成「限流冷却」）")
	}
	// 面板可见文案必须写明"会写回 + 12h 有效"，且旧口径（只观测、不写回）必须清干净 ——
	// 包括 HTML 注释：注释不改，下一个人就会照着旧口径改代码（round8 首次部署即踩到，
	// 部署脚本的验收因此报红）。
	if !strings.Contains(html, "结果写回选号器（账号级结论 12h 有效）") {
		t.Error("index.html 探针标题未写明「结果写回选号器（账号级结论 12h 有效）」")
	}
	if strings.Contains(html, "不写回池状态") || strings.Contains(html, "只观测") {
		t.Error("index.html 仍残留「只观测/不写回池状态」的旧口径（含注释）")
	}
	// 模型框占位文案是"默认档"的对外说明，必须与后端 probeRealmModels 一致 ——
	// 用户明确要求「hy4-preview 不要探测」（收费档），文案里把它列成默认档就是骗人。
	if strings.Contains(html, "hy4-preview + hy4-preview-f") {
		t.Error("index.html 占位文案仍把收费档 hy4-preview 列为默认探测档（用户明确要求不探它）")
	}

	// 表头 <th> 数 vs pbRow 的 <td> 数。
	head := html[strings.Index(html, `<tbody id="pbBody">`):]
	if i := strings.LastIndex(html[:strings.Index(html, `<tbody id="pbBody">`)], "<thead>"); i >= 0 {
		head = html[i:]
	}
	head = strings.Replace(head, "<thead>", "", 1) // "<th" 是 "<thead" 的前缀，不剔掉会多算一个
	thCount := strings.Count(head[:strings.Index(head, "</thead>")], "<th")
	rowStart := strings.Index(js, "function pbRow(r) {")
	rowEnd := strings.Index(js[rowStart:], "\n}")
	tdCount := strings.Count(js[rowStart:rowStart+rowEnd], "<td")
	if thCount != tdCount {
		t.Errorf("探针表列数不匹配：表头 %d 个 <th>，pbRow 渲染 %d 个 <td>", thCount, tdCount)
	}
}
