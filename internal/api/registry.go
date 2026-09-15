// Package api 是对外能力接口的注册表与调用实现。
//
// 设计见 docs/design/22-对外能力接口与AI可控制规范.md。
//
// 核心目标：让 AI 能轻松控制本软件。做法是提供一套
// 【可发现 + 自描述 + 统一响应 + 危险分级】的能力接口，
// 默认以 CLI/stdio 形态暴露（零常驻），HTTP 监听为可选。
//
// 本包是纯 Go 包：不 import wails、不 import main、不读全局状态，可独立单测。
package api

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Kind 是动作的危险等级。
type Kind string

const (
	// KindReadonly 只读，不改任何东西
	KindReadonly Kind = "readonly"
	// KindMutating 会改状态（写文件、起进程），但不删数据
	KindMutating Kind = "mutating"
	// KindDestructive 会删数据：默认拒绝，必须显式确认
	KindDestructive Kind = "destructive"
)

// Valid 报告等级是否合法。
func (k Kind) Valid() bool {
	return k == KindReadonly || k == KindMutating || k == KindDestructive
}

// Param 描述一个参数。
type Param struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // string / int / bool / strings
	Required bool   `json:"required"`
	Desc     string `json:"desc"`
	Example  string `json:"example,omitempty"`
}

// Action 是一个可被外部调用的能力。
type Action struct {
	ID      string // 形如 "scan.start"，必须以模块 id 为前缀
	Module  string // 归属模块（用于「每个模块必须有 action」的强制检查）
	Summary string // 一句话说明（给 AI 看，要能判断该不该调）
	Kind    Kind   // 危险等级
	Params  []Param
	// Handler 执行动作。args 为调用方传入的参数（已做必填校验）。
	// 返回值会被放进响应信封的 data 字段。
	Handler func(ctx context.Context, args map[string]any) (any, error)
}

// Info 是动作的自描述信息（对外暴露的形态）。
type Info struct {
	ID      string  `json:"id"`
	Module  string  `json:"module"`
	Kind    Kind    `json:"kind"`
	Summary string  `json:"summary"`
	Params  []Param `json:"params"`
}

// Response 是统一响应信封。
//
// 约定：失败必须带 Hint（怎么修）。AI 拿到 error 却没有 hint 就只能干瞪眼——
// 这与预检清单「未通过项必须带修复建议」是同一条原则。
type Response struct {
	OK          bool   `json:"ok"`
	Action      string `json:"action,omitempty"`
	Data        any    `json:"data,omitempty"`
	Error       string `json:"error,omitempty"`
	Hint        string `json:"hint,omitempty"`
	NeedConfirm bool   `json:"need_confirm,omitempty"`
}

// CallOptions 是一次调用选项。
type CallOptions struct {
	Args    map[string]any
	Confirm bool // 危险动作必须显式确认
}

// ErrNeedConfirm 表示动作需要显式确认。
var ErrNeedConfirm = errors.New("该动作会修改系统状态或删除数据，需要显式确认")

// Registry 是能力注册表。
type Registry struct {
	mu      sync.RWMutex
	actions map[string]*Action
	order   []string
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{actions: map[string]*Action{}}
}

// Register 注册一个动作。
//
// 重复 ID、缺少摘要、等级非法、ID 不以模块名为前缀——一律 panic。
// 这类问题属于开发期缺陷，必须在启动时暴露而不是让 AI 调到一半失败。
func (r *Registry) Register(a Action) {
	if a.ID == "" {
		panic("api: 动作 ID 不能为空")
	}
	if a.Module == "" {
		panic("api: 动作 " + a.ID + " 未声明归属模块（用于强制检查每个模块都有接口）")
	}
	if !strings.HasPrefix(a.ID, a.Module+".") {
		panic("api: 动作 " + a.ID + " 的 ID 必须以模块名 " + a.Module + ". 为前缀")
	}
	if strings.TrimSpace(a.Summary) == "" {
		panic("api: 动作 " + a.ID + " 缺少 summary（AI 靠它判断该不该调）")
	}
	if !a.Kind.Valid() {
		panic("api: 动作 " + a.ID + " 的危险等级非法: " + string(a.Kind))
	}
	if a.Handler == nil {
		panic("api: 动作 " + a.ID + " 没有 Handler")
	}
	for _, p := range a.Params {
		if p.Name == "" || p.Type == "" {
			panic("api: 动作 " + a.ID + " 的参数缺少 name/type")
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.actions[a.ID]; dup {
		panic("api: 重复注册动作 " + a.ID)
	}
	cp := a
	r.actions[a.ID] = &cp
	r.order = append(r.order, a.ID)
}

// List 返回全部动作的自描述信息（按 ID 排序，稳定输出）。
func (r *Registry) List() []Info {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := append([]string(nil), r.order...)
	sort.Strings(ids)

	out := make([]Info, 0, len(ids))
	for _, id := range ids {
		out = append(out, toInfo(r.actions[id]))
	}
	return out
}

// Describe 返回单个动作的自描述信息。
func (r *Registry) Describe(id string) (Info, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.actions[id]
	if !ok {
		return Info{}, false
	}
	return toInfo(a), true
}

// Modules 返回注册表里出现过的模块 id 集合。
func (r *Registry) Modules() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := map[string]bool{}
	var out []string
	for _, a := range r.actions {
		if !seen[a.Module] {
			seen[a.Module] = true
			out = append(out, a.Module)
		}
	}
	sort.Strings(out)
	return out
}

// HasModule 报告某模块是否已有对外接口。
func (r *Registry) HasModule(module string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, a := range r.actions {
		if a.Module == module {
			return true
		}
	}
	return false
}

// Call 执行一个动作。
//
// 危险动作在未确认时**直接拒绝并且不执行任何逻辑**——
// 这是代码行为，不是文档约定（见规范 §4 的强制测试）。
func (r *Registry) Call(ctx context.Context, id string, opts CallOptions) Response {
	r.mu.RLock()
	a, ok := r.actions[id]
	r.mu.RUnlock()

	if !ok {
		return Response{
			OK: false, Action: id,
			Error: "没有这个能力: " + id,
			Hint:  "先执行 `api list` 查看全部可用能力",
		}
	}

	if a.Kind == KindDestructive && !opts.Confirm {
		return Response{
			OK: false, Action: id, NeedConfirm: true,
			Error: ErrNeedConfirm.Error(),
			Hint:  "确认无误后重新调用并带上 --confirm",
		}
	}

	if err := validateArgs(a, opts.Args); err != nil {
		return Response{OK: false, Action: id, Error: err.Error(),
			Hint: "执行 `api describe " + id + "` 查看参数说明"}
	}

	data, err := a.Handler(ctx, opts.Args)
	if err != nil {
		return Response{OK: false, Action: id, Error: err.Error()}
	}
	return Response{OK: true, Action: id, Data: data}
}

// validateArgs 校验必填参数。
func validateArgs(a *Action, args map[string]any) error {
	for _, p := range a.Params {
		if !p.Required {
			continue
		}
		v, ok := args[p.Name]
		if !ok || v == nil {
			return fmt.Errorf("缺少必填参数 %s（%s）", p.Name, p.Desc)
		}
	}
	return nil
}

func toInfo(a *Action) Info {
	params := a.Params
	if params == nil {
		params = []Param{}
	}
	return Info{ID: a.ID, Module: a.Module, Kind: a.Kind, Summary: a.Summary, Params: params}
}
