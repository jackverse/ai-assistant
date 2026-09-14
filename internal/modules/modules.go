// Package modules 实现功能模块注册表。
//
// 设计目标（来自用户需求）：助手.exe 是一个【轻入口】，
// 各功能（扫描、清理、迁移…）是独立模块——
//   1. 配置里禁用的模块：入口不显示，后端不初始化，不做任何 IO；
//   2. 启用的模块：也只在用户点进它的页面时才初始化（懒加载）；
//   3. 新增功能 = 新增一个 Module，不改变入口的启动成本。
//
// 必须诚实说明边界：这是【单进程】工具，没有独立的后台服务进程。
// 「模块不启动」的含义是——该模块的初始化代码、数据结构与文件访问
// 都不会发生，而不是「没有进程在跑」。本工具从设计上就保证：
// 不注册开机自启、不驻留后台、扫描结束后没有任何活动（见 docs/design/01）。
package modules

import (
	"fmt"
	"sort"
	"sync"
)

// Status 描述模块的可用状态。
type Status string

const (
	// StatusReady 功能可用，可进入。
	StatusReady Status = "ready"
	// StatusDevelopment 开发中：入口可见但不可进入，不会执行任何逻辑。
	StatusDevelopment Status = "development"
	// StatusPlanned 规划中。
	StatusPlanned Status = "planned"
)

// Meta 是模块的静态描述。
//
// json tag 必须是小写：这些对象会直接序列化给前端，
// 前端统一以 snake_case 访问（与全项目其它 DTO 保持一致）。
type Meta struct {
	ID   string `json:"id"`   // 稳定标识，配置文件里用它
	Name string `json:"name"` // 显示名
	Desc string `json:"desc"` // 一句话说明
	Icon string `json:"icon"` // 展示用符号（emoji，避免引入图标资源）
	// Status 表明当前可用性
	Status Status `json:"status"`
	// DefaultEnabled 是配置未设置时的默认开关
	DefaultEnabled bool `json:"default_enabled"`
}

// OpenResult 是进入模块时的返回。
type OpenResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// Module 是一个功能模块。
//
// OpenFn 是懒加载钩子：用户第一次进入该模块页面时才被调用。
// 它应当只做「该模块自身的初始化」（例如加载自己的规则文件），
// 不允许做扫描文件系统这类重活——那是用户显式触发操作时才做的事。
type Module struct {
	Meta   Meta
	OpenFn func() (OpenResult, error)

	opened bool
}

// Registry 持有全部已注册模块与用户的启用配置。
type Registry struct {
	mu      sync.Mutex
	order   []string
	mods    map[string]*Module
	resolver func(id string, def bool) bool // 由配置文件提供；nil 时用默认值
}

// New 创建注册表。
//
// resolver 由配置层提供（一般是 config.IsEnabled 的适配），
// 返回某模块的启用状态；传 nil 则一律用模块声明的默认值。
// 用函数而非 map 的原因：配置可能热更新，注册表不必持有配置的副本。
func New(resolver func(id string, def bool) bool) *Registry {
	return &Registry{mods: map[string]*Module{}, resolver: resolver}
}

// Register 注册一个模块。重复 ID 视为程序缺陷，直接 panic——
// 这类错误应当在开发期暴露，而不是被静默忽略。
func (r *Registry) Register(m Module) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.mods[m.Meta.ID]; dup {
		panic(fmt.Sprintf("modules: 重复注册 %s", m.Meta.ID))
	}
	r.mods[m.Meta.ID] = &Module{Meta: m.Meta, OpenFn: m.OpenFn}
	r.order = append(r.order, m.Meta.ID)
}

// isEnabled 返回启用状态（不加锁版本，供 List/Open 内部使用）。
func (r *Registry) isEnabled(m *Module) bool {
	if r.resolver != nil {
		return r.resolver(m.Meta.ID, m.Meta.DefaultEnabled)
	}
	return m.Meta.DefaultEnabled
}

// List 返回模块列表（按注册顺序，稳定展示）。
//
// 禁用的模块也在列表里，但 Enabled=false——
// 入口页会以灰态显示并说明如何启用，而不是凭空消失：
// 用户需要知道「这个功能存在但我关了」，否则配置开关等于没做。
func (r *Registry) List() []ModuleView {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]ModuleView, 0, len(r.order))
	for _, id := range r.order {
		m := r.mods[id]
		out = append(out, ModuleView{
			Meta:      m.Meta,
			Enabled:   r.isEnabled(m),
			Opened:    m.opened,
			ConfigKey: fmt.Sprintf("modules.%s.enabled", id),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.ID < out[j].Meta.ID })
	return out
}

// ModuleView 是给前端的模块视图。
type ModuleView struct {
	Meta      Meta   `json:"meta"`
	Enabled   bool   `json:"enabled"`
	Opened    bool   `json:"opened"`
	ConfigKey string `json:"config_key"`
}

// Open 进入模块：首次调用会触发懒加载钩子。
//
// 三种拒绝路径都必须给出可读原因：
//   - 模块不存在
//   - 模块在配置中被禁用
//   - 模块尚未开发完成（Status != ready）
func (r *Registry) Open(id string) (OpenResult, error) {
	r.mu.Lock()
	m := r.mods[id]
	if m == nil {
		r.mu.Unlock()
		return OpenResult{}, fmt.Errorf("未知模块: %s", id)
	}
	if !r.isEnabled(m) {
		r.mu.Unlock()
		return OpenResult{OK: false,
			Message: fmt.Sprintf("模块 %s 已在配置中禁用（%s.enabled = false）。编辑 %%APPDATA%%\\winclean\\config.yaml 后重启程序可启用",
				m.Meta.Name, id)}, nil
	}
	if m.Meta.Status != StatusReady {
		r.mu.Unlock()
		return OpenResult{OK: false,
			Message: fmt.Sprintf("模块 %s 尚未完成（状态: %s）", m.Meta.Name, m.Meta.Status)}, nil
	}

	first := !m.opened
	var fn = m.OpenFn
	r.mu.Unlock()

	if first && fn != nil {
		res, err := fn()
		if err != nil {
			return OpenResult{OK: false, Message: "初始化失败: " + err.Error()}, err
		}
		r.mu.Lock()
		m.opened = true
		r.mu.Unlock()
		return res, nil
	}

	r.mu.Lock()
	m.opened = true
	r.mu.Unlock()
	return OpenResult{OK: true, Message: ""}, nil
}

// EnabledCount 返回启用的模块数（入口页判断「至少有一个可用」时用）。
func (r *Registry) EnabledCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, id := range r.order {
		if r.isEnabled(r.mods[id]) {
			n++
		}
	}
	return n
}

// SetEnabled 修改运行中的启用状态，并返回是否找到了该模块。
//
// 供界面上的开关调用。持久化由调用方写配置文件完成——
// 注册表通过 resolver 从配置层读取状态，配置一改这里即生效，
// 因此注册表自身不保存第二份开关状态（避免两处真相不一致）。
// 启用一个未打开过的模块不会立即初始化它——仍然等到真正进入页面，
// 这样「开了开关但没点进去」依然零开销。
func (r *Registry) SetEnabled(id string, enabled bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.mods[id]
	if m == nil {
		return false
	}
	if !enabled {
		m.opened = false // 禁用时视为已关闭，下次启用重新走懒加载
	}
	return true
}
