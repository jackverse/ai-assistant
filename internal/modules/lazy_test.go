package modules

import (
	"strings"
	"sync/atomic"
	"testing"
)

// 本文件把「不加载就不造成运行负担」（docs/design/20）变成可执行的断言。
//
// 核心不变式：OpenFn 只在【用户真正进入模块】时被调用，且恰好一次——
// 注册、列列表、数启用数、改开关，都不得触发它。
//
// 边界说明：本文件测的是注册表自身的行为。某个具体模块的 OpenFn 里
// 是否偷偷做了重活（遍历文件系统、探测工具链），这里测不出来——
// 那要靠 docs/design/20 §7 的四项人工验证。

// countingModule 返回一个模块与一个调用计数器。
func countingModule(id string, status Status, def bool) (Module, *atomic.Int32) {
	var calls atomic.Int32
	return Module{
		Meta: Meta{
			ID:             id,
			Name:           "测试模块 " + id,
			Desc:           "仅用于测试",
			Icon:           "🧪",
			Status:         status,
			DefaultEnabled: def,
		},
		OpenFn: func() (OpenResult, error) {
			calls.Add(1)
			return OpenResult{OK: true}, nil
		},
	}, &calls
}

// 注册、列列表、数启用数——全是元数据操作，一次也不许初始化。
func TestListAndCountDoNotLoad(t *testing.T) {
	reg := New(nil)
	m, calls := countingModule("alpha", StatusReady, true)
	reg.Register(m)

	views := reg.List()
	if len(views) != 1 {
		t.Fatalf("List 应返回 1 个模块，实际 %d", len(views))
	}
	if got := reg.EnabledCount(); got != 1 {
		t.Fatalf("EnabledCount 应为 1，实际 %d", got)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("只查看列表就触发了 %d 次初始化——违反不加载规则", n)
	}
	if views[0].Opened {
		t.Fatal("未进入的模块不应标记为已打开")
	}
}

// Open 触发一次；重复 Open 不再触发（重进同一页面不该重新初始化）。
func TestOpenLoadsExactlyOnce(t *testing.T) {
	reg := New(nil)
	m, calls := countingModule("alpha", StatusReady, true)
	reg.Register(m)

	if _, err := reg.Open("alpha"); err != nil {
		t.Fatalf("首次 Open 不应报错: %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("首次 Open 应初始化 1 次，实际 %d", n)
	}
	if _, err := reg.Open("alpha"); err != nil {
		t.Fatalf("再次 Open 不应报错: %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("再次 Open 不应重复初始化，实际 %d 次", n)
	}
}

// 被禁用的模块：拒绝进入、给出可读原因、且绝不初始化。
func TestDisabledModuleNeverLoads(t *testing.T) {
	reg := New(func(string, bool) bool { return false })
	m, calls := countingModule("alpha", StatusReady, true)
	reg.Register(m)

	res, err := reg.Open("alpha")
	if err != nil {
		t.Fatalf("禁用是预期状态，不应作为错误返回: %v", err)
	}
	if res.OK {
		t.Fatal("禁用的模块不应允许进入")
	}
	if !strings.Contains(res.Message, "禁用") || !strings.Contains(res.Message, "alpha.enabled") {
		t.Fatalf("拒绝原因必须可读且指出怎么启用，实际: %q", res.Message)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("禁用的模块被初始化了 %d 次", n)
	}
}

// 未开发完的模块：入口可见但不可进入，同样零初始化。
func TestNotReadyModuleNeverLoads(t *testing.T) {
	for _, status := range []Status{StatusDevelopment, StatusPlanned} {
		reg := New(nil)
		m, calls := countingModule("alpha", status, true)
		reg.Register(m)

		res, err := reg.Open("alpha")
		if err != nil {
			t.Fatalf("%s: 不应返回错误: %v", status, err)
		}
		if res.OK {
			t.Fatalf("%s: 未完成的模块不应允许进入", status)
		}
		if n := calls.Load(); n != 0 {
			t.Fatalf("%s: 被初始化了 %d 次", status, n)
		}
	}
}

// 未知 id 是调用方的缺陷，必须报错而不是静默通过。
func TestOpenUnknownModuleFails(t *testing.T) {
	reg := New(nil)
	if _, err := reg.Open("nope"); err == nil {
		t.Fatal("未知模块应返回错误")
	}
}

// 禁用 → 重新启用后：必须重新走懒加载（用户又要进来了，该重建的状态要重建）。
//
// 注意启用状态由 resolver（配置层）决定，SetEnabled 只负责重置已打开标记，
// 因此这里通过翻动 resolver 背后的变量来模拟用户改配置。
func TestReEnableAfterDisableLoadsAgain(t *testing.T) {
	enabled := true
	reg := New(func(string, bool) bool { return enabled })
	m, calls := countingModule("alpha", StatusReady, true)
	reg.Register(m)

	if _, err := reg.Open("alpha"); err != nil {
		t.Fatalf("Open 报错: %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("首次进入应初始化 1 次，实际 %d", n)
	}

	enabled = false
	if !reg.SetEnabled("alpha", false) {
		t.Fatal("SetEnabled 应找到该模块")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("关闭开关不应触发初始化，实际 %d 次", n)
	}

	if res, err := reg.Open("alpha"); err != nil || res.OK {
		t.Fatalf("禁用状态下应拒绝进入，res=%+v err=%v", res, err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("禁用状态下 Open 不应初始化，实际 %d 次", n)
	}

	enabled = true
	if _, err := reg.Open("alpha"); err != nil {
		t.Fatalf("重新启用后 Open 报错: %v", err)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("重新启用后应重新初始化，期望 2 次，实际 %d 次", n)
	}
}

// 模块没有 OpenFn 也应能正常进入（不 panic）。
func TestModuleWithoutOpenFn(t *testing.T) {
	reg := New(nil)
	reg.Register(Module{Meta: Meta{ID: "alpha", Name: "无钩子", Status: StatusReady, DefaultEnabled: true}})
	res, err := reg.Open("alpha")
	if err != nil || !res.OK {
		t.Fatalf("无 OpenFn 的模块应可直接进入，res=%+v err=%v", res, err)
	}
}

// 重复注册是程序缺陷，必须在开发期炸出来而不是静默覆盖。
func TestDuplicateRegisterPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("重复注册同一个 id 应当 panic")
		}
	}()
	reg := New(nil)
	m1, _ := countingModule("alpha", StatusReady, true)
	m2, _ := countingModule("alpha", StatusReady, true)
	reg.Register(m1)
	reg.Register(m2)
}
