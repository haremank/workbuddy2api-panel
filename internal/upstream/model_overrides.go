// model_overrides.go 模型名单的运维补充/屏蔽表（config.models）。
//
// 为什么需要它：上游目录与"实际可调"并不一致（2026-09-21 逐模型实测）：
//   - CN 侧有 5 个模型上游能调通、但不在 /v1/models 里（网关按 agents[cli]
//     过滤掉了）：deepseek-v3-2-volc / default / hunyuan-2.0-thinking /
//     hunyuan-chat / kimi-k2.5；
//   - global 侧有 2 个列在目录里、上游却恒 11102：o4-mini / auto-chat。
//
// 两者都不是 realm 过滤能解决的，只能靠一张显式表。语义上它是**纯补充**：
// 动态探测失败仍返回空名单（不引入"静态兜底"，与既有纯动态口径不冲突）。
package upstream

import "strings"

// ModelOverrides 模型名单的补充（Extra）与屏蔽（Hide）表，来自 config.models。
// 零值 = 不做任何改动（对老配置零影响）。
type ModelOverrides struct {
	Extra []string
	Hide  []string
}

// IsZero 报告是否未配置（用于跳过整段处理）。
func (o ModelOverrides) IsZero() bool { return len(o.Extra) == 0 && len(o.Hide) == 0 }

// Apply 先按 Hide 剔除、再按 Extra 追加。
// 顺序：动态结果保持原序在前，补充项按配置顺序追加在后；按 id 去重。
// 入参 ids 不被修改（返回新切片），调用方可安全传入缓存切片。
func (o ModelOverrides) Apply(ids []string) []string {
	if o.IsZero() {
		return ids
	}
	hide := make(map[string]bool, len(o.Hide))
	for _, h := range o.Hide {
		if h = strings.TrimSpace(h); h != "" {
			hide[h] = true
		}
	}
	out := make([]string, 0, len(ids)+len(o.Extra))
	seen := make(map[string]bool, len(ids)+len(o.Extra))
	for _, id := range ids {
		if id == "" || hide[id] || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	for _, id := range o.Extra {
		if id = strings.TrimSpace(id); id == "" || hide[id] || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
