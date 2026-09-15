package main

// 对外接口的强制检查（规范见 docs/design/22-对外能力接口与AI可控制规范.md §4）。
//
// 三条断言把「所有功能都要有 API 接口」从文档承诺变成可验证的代码行为。

import (
	"context"
	"strings"
	"testing"

	"winclean/internal/api"
	"winclean/internal/config"
)

// TestEveryModuleHasAction 每个已注册模块都必须至少暴露一个对外动作。
//
// 漏了说明有功能只能从界面点、AI 控制不了——那正是这条规则要禁止的。
func TestEveryModuleHasAction(t *testing.T) {
	app := NewApp(config.Default())
	reg := api.NewRegistry()
	registerAllActions(app, reg)

	var missing []string
	for _, m := range app.reg.List() {
		if !reg.HasModule(m.Meta.ID) {
			missing = append(missing, m.Meta.ID)
		}
	}
	if len(missing) > 0 {
		t.Errorf("以下模块没有对外接口（AI 无法控制）: %v\n"+
			"修法：在 api_actions.go 的 register<X>Actions 里为它注册动作", missing)
	}

	// 反向检查：注册表里的模块都应真实存在，避免拼错模块名绕过检查
	known := map[string]bool{}
	for _, m := range app.reg.List() {
		known[m.Meta.ID] = true
	}
	for _, mod := range reg.Modules() {
		if !known[mod] {
			t.Errorf("接口注册表里出现未知模块 %q（拼写错误？）", mod)
		}
	}
}

// TestEveryActionIsDescribed 每个动作必须自描述——AI 靠这些信息判断该不该调用。
func TestEveryActionIsDescribed(t *testing.T) {
	app := NewApp(config.Default())
	reg := api.NewRegistry()
	registerAllActions(app, reg)

	actions := reg.List()
	if len(actions) == 0 {
		t.Fatal("接口注册表为空")
	}

	for _, a := range actions {
		if strings.TrimSpace(a.Summary) == "" {
			t.Errorf("动作 %s 缺少 summary（AI 无法判断用途）", a.ID)
		}
		if !a.Kind.Valid() {
			t.Errorf("动作 %s 的危险等级非法: %s", a.ID, a.Kind)
		}
		if !strings.HasPrefix(a.ID, a.Module+".") {
			t.Errorf("动作 %s 的 ID 未以模块名 %s. 为前缀", a.ID, a.Module)
		}
		for _, p := range a.Params {
			if p.Type == "" {
				t.Errorf("动作 %s 的参数 %s 缺少类型", a.ID, p.Name)
			}
			if p.Desc == "" {
				t.Errorf("动作 %s 的参数 %s 缺少说明", a.ID, p.Name)
			}
			// 必填参数应给示例，否则 AI 容易猜错格式
			if p.Required && p.Example == "" {
				t.Errorf("动作 %s 的必填参数 %s 缺少 example", a.ID, p.Name)
			}
		}
	}
}

// TestDestructiveRequiresConfirm 危险动作未确认时必须拒绝，且无任何副作用。
//
// 这条把「危险动作默认拒绝」从文档承诺变成可断言的代码行为——
// 是本次规则里最要紧的一条。
func TestDestructiveRequiresConfirm(t *testing.T) {
	reg := api.NewRegistry()

	executed := false
	reg.Register(api.Action{
		ID: "disk.fake_delete", Module: "disk", Kind: api.KindDestructive,
		Summary: "测试用：假装删除",
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			executed = true
			return "deleted", nil
		},
	})

	// 未确认 → 拒绝
	resp := reg.Call(context.Background(), "disk.fake_delete", api.CallOptions{})
	if resp.OK {
		t.Error("危险动作未确认时不该执行成功")
	}
	if !resp.NeedConfirm {
		t.Error("应返回 need_confirm=true 让调用方知道要加确认")
	}
	if resp.Hint == "" {
		t.Error("拒绝时必须给出 hint（怎么确认）")
	}
	if executed {
		t.Error("未确认时绝不能执行 handler——这是安全底线")
	}

	// 确认后 → 执行
	resp = reg.Call(context.Background(), "disk.fake_delete", api.CallOptions{Confirm: true})
	if !resp.OK || !executed {
		t.Error("确认后应正常执行")
	}
}

// TestRealDestructiveActionsAreMarked 真实注册表里的删除类动作必须标为 destructive。
//
// 防止有人图省事把会删数据的动作标成 readonly/mutating，绕过确认机制。
func TestRealDestructiveActionsAreMarked(t *testing.T) {
	app := NewApp(config.Default())
	reg := api.NewRegistry()
	registerAllActions(app, reg)

	// 按 id 关键字判断哪些动作必然涉及删除/移动数据
	mustBeDestructive := []string{"clean_execute", "migrate_execute"}
	for _, a := range reg.List() {
		for _, kw := range mustBeDestructive {
			if strings.Contains(a.ID, kw) && a.Kind != api.KindDestructive {
				t.Errorf("动作 %s 会删/移数据，危险等级必须是 destructive，实际 %s", a.ID, a.Kind)
			}
		}
	}
}

// TestMissingActionGivesHint 调用不存在的动作必须给出可操作的提示。
func TestMissingActionGivesHint(t *testing.T) {
	app := NewApp(config.Default())
	reg := api.NewRegistry()
	registerAllActions(app, reg)

	resp := reg.Call(context.Background(), "not.exist", api.CallOptions{})
	if resp.OK || resp.Hint == "" {
		t.Error("未知动作必须失败并提示如何列出可用能力")
	}
}

// TestMissingRequiredParamIsRejected 缺少必填参数时给出参数说明入口。
func TestMissingRequiredParamIsRejected(t *testing.T) {
	app := NewApp(config.Default())
	reg := api.NewRegistry()
	registerAllActions(app, reg)

	// disk.scan_start 的 disk 是必填
	resp := reg.Call(context.Background(), "disk.scan_start", api.CallOptions{})
	if resp.OK {
		t.Error("缺少必填参数不该执行成功")
	}
	if resp.Hint == "" {
		t.Error("缺少参数时必须提示如何查看参数说明")
	}
}
