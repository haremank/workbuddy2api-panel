package upstream

import (
	"reflect"
	"testing"
)

func TestModelOverridesZeroIsNoop(t *testing.T) {
	var o ModelOverrides
	in := []string{"a", "b"}
	if got := o.Apply(in); !reflect.DeepEqual(got, in) {
		t.Fatalf("零值应原样返回，得到 %v", got)
	}
	if !o.IsZero() {
		t.Fatal("零值 IsZero 应为 true")
	}
}

func TestModelOverridesHideThenExtra(t *testing.T) {
	o := ModelOverrides{
		Hide:  []string{"o4-mini", " auto-chat "}, // 带空白也应生效
		Extra: []string{"kimi-k2.5", "hy3"},
	}
	in := []string{"o4-mini", "auto-chat", "hy3", "glm-5.3"}
	// hy3 已在动态结果里 → Extra 不重复追加（保持原位置）。
	want := []string{"hy3", "glm-5.3", "kimi-k2.5"}
	if got := o.Apply(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}

func TestModelOverridesExtraOnlyAppendsInOrder(t *testing.T) {
	o := ModelOverrides{Extra: []string{"z", "a", "", "  "}}
	in := []string{"m"}
	want := []string{"m", "z", "a"} // 空串/空白项被丢弃，顺序按配置
	if got := o.Apply(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}

// 动态探测失败（空名单）时补充项仍生效：这是运维显式声明的"已知可调"名单，
// 不随一次探测失败而消失。
func TestModelOverridesExtraSurvivesEmptyDynamic(t *testing.T) {
	o := ModelOverrides{Extra: []string{"kimi-k2.5"}}
	if got := o.Apply(nil); !reflect.DeepEqual(got, []string{"kimi-k2.5"}) {
		t.Fatalf("空输入应只剩补充项，得到 %v", got)
	}
}

// Apply 不得修改入参切片：调用方可能传入 1h 缓存的底层数组。
func TestModelOverridesDoesNotMutateInput(t *testing.T) {
	o := ModelOverrides{Hide: []string{"b"}, Extra: []string{"c"}}
	in := []string{"a", "b"}
	orig := append([]string(nil), in...)
	_ = o.Apply(in)
	if !reflect.DeepEqual(in, orig) {
		t.Fatalf("入参被改写：%v", in)
	}
}
