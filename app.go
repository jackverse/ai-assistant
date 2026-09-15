package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"winclean/internal/ai"
	"winclean/internal/clean"
	"winclean/internal/cli"
	"winclean/internal/config"
	"winclean/internal/deploy"
	"winclean/internal/migrate"
	"winclean/internal/model"
	"winclean/internal/modules"
	"winclean/internal/report"
	"winclean/internal/scan"
	"winclean/internal/sys"
	"winclean/internal/version"
	"winclean/internal/winapi"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// procStart 用于度量「界面就绪耗时」，向用户证明入口足够轻。
var procStart = time.Now()

// App 是暴露给前端的后端对象。
//
// 架构约定（14 号设计文档）：主功能 1 个 + 扩展功能 N 个。
// App 自身只持有入口必需的状态（配置、模块注册表、当前扫描/对话会话）；
// 任何模块的后端资源都在用户进入该模块页面时才创建。
// AI 模块在未配置时不会产生任何网络活动。
type App struct {
	ctx context.Context

	cfg *config.Config
	reg *modules.Registry

	mu      sync.Mutex
	cancel  context.CancelFunc
	prog    *scan.Progress
	result  *model.ScanResult
	running atomic.Bool
	done    atomic.Bool
	lastErr atomic.Value
	started time.Time

	chatMu     sync.Mutex
	chatCancel context.CancelFunc
	chatBusy   atomic.Bool

	bootWarnings []string

	// ── 垃圾清理模块状态（clean_api.go）──
	cleanMu        sync.Mutex
	cleanBusy      bool
	cleanCancel    context.CancelFunc
	cleanScanDone  atomic.Bool
	cleanScanErr   atomic.Value // string
	cleanScanCur   atomic.Value // string
	cleanScanCount atomic.Int64
	cleanExecDone  atomic.Bool
	cleanExecErr   atomic.Value // string
	cleanExecName  atomic.Value // string
	cleanExecFreed atomic.Int64
	cleanExecFiles atomic.Int64
	cleanItems     []clean.Candidate
	cleanStats     map[string]clean.Stats
	cleanResults   []clean.Result

	// ── 空间迁移模块状态（migrate_api.go）──
	migMu          sync.Mutex
	migBusy        bool
	migCancel      context.CancelFunc
	migCustomSeq   int
	migScanDone    atomic.Bool
	migScanErr     atomic.Value // string
	migScanCur     atomic.Value // string
	migScanCount   atomic.Int64
	migExecDone    atomic.Bool
	migExecErr     atomic.Value // string
	migPhase       atomic.Value // string
	migItemName    atomic.Value // string
	migCopiedBytes atomic.Int64
	migCopiedFiles atomic.Int64
	migCurFile     atomic.Value // string
	migCands       []migrate.Cand
	migResults     []migrate.ItemResult
}

// NewApp 创建应用对象并注册全部功能模块。
//
// 新增扩展页的方式：在这里 Register 一个 Module，再在前端加对应页面。
// 入口的启动成本不随模块数量增长——注册只是往 map 放一条元数据。
func NewApp(cfg *config.Config) *App {
	a := &App{cfg: cfg, prog: scan.NewProgress()}
	a.reg = modules.New(cfg.IsEnabled)
	appRef = a

	registerModules()
	return a
}

// appRef 单窗口应用的引用（模块 OpenFn 需要读取配置等 App 资源时使用）。
var appRef *App

// registerModules 声明功能模块。
//
// 主/副的区分不在注册表——聊天与扫描在架构上完全平等，
// 「谁是主页」由 config.general.home 指向谁决定。
func registerModules() {
	r := appRef.reg

	r.Register(modules.Module{
		Meta: modules.Meta{
			ID:             "chat",
			Name:           "AI 助手",
			Desc:           "与大模型对话，由它给出各功能的快捷入口",
			Icon:           "💬",
			Status:         modules.StatusReady,
			DefaultEnabled: true,
		},
		OpenFn: func() (modules.OpenResult, error) {
			// 聊天模块的「初始化」是零成本的：不建连接、不探测网络、不校验配置。
			// 配置检查在聊天页展示时进行；只有用户真正发送消息才会发起 HTTP 请求。
			return modules.OpenResult{OK: true}, nil
		},
	})

	r.Register(modules.Module{
		Meta: modules.Meta{
			ID:             "disk",
			Name:           "磁盘整理",
			Desc:           "空间扫描 · 垃圾清理 · 空间迁移，三合一",
			Icon:           "🔍",
			Status:         modules.StatusReady,
			DefaultEnabled: true,
		},
		OpenFn: func() (modules.OpenResult, error) {
			// 刻意不做任何文件系统遍历——测量/清理/迁移都发生在用户
			// 在页签里显式点击对应按钮时。
			if _, err := sys.EnumerateVolumes(); err != nil {
				return modules.OpenResult{OK: false, Message: "枚举磁盘失败: " + err.Error()}, err
			}
			return modules.OpenResult{OK: true,
				Message: "扫描引擎就绪（仅读取磁盘清单与环境信息，未扫描任何文件）"}, nil
		},
	})

	r.Register(modules.Module{
		Meta: modules.Meta{
			ID:             "deploy",
			Name:           "Java 开发",
			Desc:           "单独启停服务 · 加载依赖 · 查看 Git 变更 · 打包产物",
			Icon:           "🛠",
			Status:         modules.StatusReady,
			DefaultEnabled: true,
		},
		OpenFn: func() (modules.OpenResult, error) {
			// 首次进入才做的事：把服务运行时的事件接上事件总线。
			// 这不产生 IO、不起协程、不查端口——只是一次函数指针赋值；
			// 放在这里而不是 startup()，是为了让「从没进过本页面的用户」
			// 连这点连接都不建立（见 AGENTS.md §2.1 第 5 条）。
			if appRef != nil {
				appRef.initDevEvents()
			}
			return modules.OpenResult{OK: true}, nil
		},
	})
}

// startup 由 Wails 在窗口就绪后调用。
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	// 配置自愈：把历史遗留的、写错的模块 Root 改写为正确值并落盘。
	// 放在启动时做的原因——这样用户一打开界面看到的路径就是对的，
	// 而不是每次靠运行期容错兜着、配置里却一直是错值。
	// 开销极小：只对已配置的模块做几次目录存在性检查。
	if deploy.NormalizeConfig(&a.cfg.Deploy, a.cfg.Deploy.BasePath) {
		if err := a.cfg.Save(); err != nil {
			a.bootWarnings = append(a.bootWarnings,
				"部署配置已自动修正，但写回配置文件失败："+err.Error())
		} else {
			a.bootWarnings = append(a.bootWarnings,
				"已自动修正部署配置里写错的模块路径（原路径会拼出重复的项目目录）")
		}
	}
}

// SetBootWarnings 记录启动阶段的非致命问题（如配置损坏已回退默认）。
func (a *App) SetBootWarnings(w []string) { a.bootWarnings = w }

// BootWarnings 返回启动阶段的问题。
func (a *App) BootWarnings() []string { return a.bootWarnings }

// BootMS 返回从进程启动到界面就绪的毫秒数。
//
// 暴露这个数字是刻意的：用户要求「不拖慢启动」，那就把启动耗时摆在明面上，
// 以后任何功能进来如果拖慢了它，立刻能被发现。
func (a *App) BootMS() int64 { return time.Since(procStart).Milliseconds() }

// ───────── 主页解析与设置 ─────────

// ModuleDTO / OpenModuleResult 是暴露给前端的模块视图。
//
// 刻意定义在 main 包而不是直接复用 internal/modules 的类型：
// 实测（本机 GUI 验证）当绑定方法的签名直接引用 internal/modules 包的类型
// （如 `Modules() []modules.ModuleView`）时，Wails 的运行时绑定会静默丢弃
// 该方法（window.go.main.App 上没有它，调用即 "is not a function"），
// 而同文件里引用 internal/model、internal/cli、internal/ai 类型的方法都正常。
// 用 main 包 DTO 做一层转换即可绕开；顺带让前端与内部模型解耦。
type ModuleDTO struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Desc      string `json:"desc"`
	Icon      string `json:"icon"`
	Status    string `json:"status"`
	Enabled   bool   `json:"enabled"`
	Opened    bool   `json:"opened"`
	ConfigKey string `json:"config_key"`
}

type OpenModuleResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

func toModuleDTO(m modules.ModuleView) ModuleDTO {
	return ModuleDTO{
		ID: m.Meta.ID, Name: m.Meta.Name, Desc: m.Meta.Desc, Icon: m.Meta.Icon,
		Status: string(m.Meta.Status), Enabled: m.Enabled, Opened: m.Opened,
		ConfigKey: m.ConfigKey,
	}
}

// SettingsDTO 是设置页需要的全部信息。
type SettingsDTO struct {
	Home       string           `json:"home"`
	Pages      []HomePageOption `json:"pages"`
	AI         AISettingsDTO    `json:"ai"`
	ConfigPath string           `json:"config_path"`
	Modules    []ModuleDTO      `json:"modules"`
}

type HomePageOption struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Current bool   `json:"current"`
	Enabled bool   `json:"enabled"`
}

type AISettingsDTO struct {
	Configured bool   `json:"configured"`
	Provider   string `json:"provider"`
	BaseURL    string `json:"base_url"`
	Model      string `json:"model"`
	HasAPIKey  bool   `json:"has_api_key"`
}

// GetSettings 返回设置页数据。
func (a *App) GetSettings() SettingsDTO {
	s := SettingsDTO{ConfigPath: a.ConfigPath()}

	// 主页候选：所有已启用模块
	for _, m := range a.reg.List() {
		if !m.Enabled {
			continue
		}
		s.Pages = append(s.Pages, HomePageOption{
			ID: m.Meta.ID, Name: m.Meta.Name + " " + m.Meta.Icon,
			Current: false, Enabled: true,
		})
	}

	home := a.resolveHome()
	s.Home = home
	for i := range s.Pages {
		s.Pages[i].Current = s.Pages[i].ID == home
	}

	c := a.cfg.AI
	s.AI = AISettingsDTO{
		Provider:  c.Provider,
		BaseURL:   c.BaseURL,
		Model:     c.Model,
		HasAPIKey: c.APIKey != "",
	}
	s.AI.Configured = ai.Settings{BaseURL: c.BaseURL, APIKey: c.APIKey, Model: c.Model}.Valid()

	for _, m := range a.reg.List() {
		s.Modules = append(s.Modules, toModuleDTO(m))
	}
	return s
}

// ListModules 返回模块列表（含禁用的，前端以灰态展示并说明如何启用）。
func (a *App) ListModules() []ModuleDTO {
	list := a.reg.List()
	out := make([]ModuleDTO, 0, len(list))
	for _, m := range list {
		out = append(out, toModuleDTO(m))
	}
	return out
}

// EnterModule 进入模块。首次进入触发该模块的懒加载初始化。
func (a *App) EnterModule(id string) (OpenModuleResult, error) {
	res, err := a.reg.Open(id)
	return OpenModuleResult{OK: res.OK, Message: res.Message}, err
}

// resolveHome 解析启动主页，配置指向不可用页面时回退到第一个可用页。
//
// 回退必须在后端做：前端如果只拿到一个不存在的 id，就只能渲染空白。
func (a *App) resolveHome() string {
	want := a.cfg.General.Home

	list := a.reg.List()
	byID := map[string]modules.ModuleView{}
	var firstReady string
	for _, m := range list {
		byID[m.Meta.ID] = m
		if firstReady == "" && m.Enabled && m.Meta.Status == modules.StatusReady {
			firstReady = m.Meta.ID
		}
	}

	if m, ok := byID[want]; ok && m.Enabled {
		return want
	}

	// 配置里的主页不可用（或从未设置）时的回退顺序：
	//   AI 已配置 → 聊天页（它是这个形态下的主功能）
	//   否则      → 磁盘整理（工具形态的门面；绝不能拿一个
	//               「需要先配置才能用」的页面当启动首页）
	if m, ok := byID["chat"]; ok && m.Enabled && a.aiSettings().Valid() {
		return "chat"
	}
	if _, ok := byID["disk"]; ok && byID["disk"].Enabled {
		return "disk"
	}
	return firstReady
}

// HomePage 返回启动时应显示的页面 id。
func (a *App) HomePage() string { return a.resolveHome() }

// SetHome 设置启动主页并持久化。
func (a *App) SetHome(id string) error {
	found := false
	for _, m := range a.reg.List() {
		if m.Meta.ID == id && m.Enabled {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("不能把主页设为 %s：该页面不存在或已被禁用", id)
	}
	a.cfg.General.Home = id
	return a.cfg.Save()
}

// SetModuleEnabled 开/关某个模块并写入配置文件。
//
// 这就是「需要用清理的时候把清理打开」的落地：
// 开关即配置，配置即持久化，重启后依然生效。
// 关闭的是当前主页时，主页自动回退（下次启动按 resolveHome 生效）。
func (a *App) SetModuleEnabled(id string, enabled bool) (ModuleDTO, error) {
	if !a.reg.SetEnabled(id, enabled) {
		return ModuleDTO{}, fmt.Errorf("未知模块: %s", id)
	}
	a.cfg.SetEnabled(id, enabled)
	if err := a.cfg.Save(); err != nil {
		return ModuleDTO{}, fmt.Errorf("写入配置失败: %w", err)
	}
	// 禁用「Java 开发」时把它启动的服务一并停掉。
	// 规则是「模块被禁用后必须回到从未加载的状态」（AGENTS.md §2.1 第 9 条），
	// 留下几个由它启动、界面又再也管不到的 java 进程，显然不算。
	if !enabled && id == "deploy" {
		devMgr().StopAll()
	}
	for _, m := range a.reg.List() {
		if m.Meta.ID == id {
			return toModuleDTO(m), nil
		}
	}
	return ModuleDTO{}, nil
}

// RefreshConfig 从磁盘重新加载配置。
//
// 场景：程序运行期间用户手改了 config.yaml，或在别处修改后切回本程序——
// 内存里的配置是启动时读的，不刷新就一直是旧值。
// 必须按字段原地更新而非替换 a.cfg 指针：模块注册表的 resolver
// 持有的是这个指针，整体替换会让运行时开关与文件脱钩。
func (a *App) RefreshConfig() error {
	nc, _, err := config.Load()
	if err != nil {
		return err
	}
	a.cfg.General = nc.General
	a.cfg.AI = nc.AI
	a.cfg.Deploy = nc.Deploy
	a.cfg.Modules = nc.Modules
	return nil
}

// ConfigPath 返回配置文件路径，供界面提示用户可手动编辑。
func (a *App) ConfigPath() string {
	p, err := config.Path()
	if err != nil {
		return "(无法确定配置路径)"
	}
	return p
}

// Version 返回版本信息。
func (a *App) Version() string { return version.String() }

// ───────── AI 设置与对话 ─────────

func (a *App) aiSettings() ai.Settings {
	c := a.cfg.AI
	return ai.Settings{BaseURL: c.BaseURL, APIKey: c.APIKey, Model: c.Model}
}

// AIPresets 返回服务商预设（设置页下拉）。
func (a *App) AIPresets() []ai.Preset { return ai.Presets() }

// SaveAISettings 保存 AI 接入配置并持久化。
//
// apiKey 传空表示「保持原值不变」——编辑设置时前端不回显密钥，
// 避免明文密钥在界面与日志里来回出现。
func (a *App) SaveAISettings(provider, baseURL, model, apiKey string) error {
	baseURL = strings.TrimSpace(baseURL)
	model = strings.TrimSpace(model)
	if baseURL != "" && !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return errors.New("base_url 必须以 http:// 或 https:// 开头")
	}

	a.cfg.AI.Provider = strings.TrimSpace(provider)
	a.cfg.AI.BaseURL = baseURL
	a.cfg.AI.Model = model
	if apiKey != "" {
		a.cfg.AI.APIKey = strings.TrimSpace(apiKey)
	}
	return a.cfg.Save()
}

// TestAI 用一条极短消息验证配置。只在用户点「测试连接」时发起请求。
func (a *App) TestAI() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return ai.Test(ctx, a.aiSettings())
}

// ChatSend 发起一次流式对话。
//
// 增量通过事件推送：chat:delta {id, delta}；结束时 chat:done {id, content, error}。
// 历史由前端持有并整体传入——后端不保存对话记录，
// 这样「关闭程序即清空」的隐私语义清晰，也避免后端状态膨胀。
func (a *App) ChatSend(id string, messages []ai.Message) error {
	if !a.chatBusy.CompareAndSwap(false, true) {
		return errors.New("上一条回复还在生成中")
	}
	s := a.aiSettings()
	if !s.Valid() {
		a.chatBusy.Store(false)
		return errors.New("AI 尚未配置：请先到「设置」填写接口信息，或把主页切换为磁盘扫描")
	}
	if len(messages) == 0 {
		a.chatBusy.Store(false)
		return errors.New("消息为空")
	}
	// 限制历史长度，避免长对话把请求体撑爆（约 60 条足够）
	if len(messages) > 60 {
		messages = messages[len(messages)-60:]
	}

	ctx, cancel := context.WithCancel(context.Background())
	a.chatMu.Lock()
	a.chatCancel = cancel
	a.chatMu.Unlock()

	go func() {
		defer a.chatBusy.Store(false)
		defer cancel()

		emitDelta := func(delta string) {
			if a.ctx != nil {
				runtime.EventsEmit(a.ctx, "chat:delta", map[string]string{"id": id, "delta": delta})
			}
		}

		content, err := ai.Stream(ctx, s, messages, emitDelta)

		errMsg := ""
		if err != nil {
			if ctx.Err() != nil {
				errMsg = "（已停止生成）"
				content += errMsg
			} else {
				errMsg = err.Error()
			}
		}
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "chat:done", map[string]string{
				"id": id, "content": content, "error": errMsg,
			})
		}
	}()

	return nil
}

// ChatStop 停止正在生成的回复。
func (a *App) ChatStop() {
	a.chatMu.Lock()
	cancel := a.chatCancel
	a.chatMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// ───────── 扫描（扩展页之一，逻辑与 CLI 完全同源） ─────────

// ProgressDTO 是前端轮询的进度快照。
//
// 刻意用「前端轮询」而不是「后端推送事件」：轮询的实现更简单、
// 时序更可控，不会出现事件在监听器注册前发出而丢失的问题。
type ProgressDTO struct {
	Running    bool   `json:"running"`
	Done       bool   `json:"done"`
	Error      string `json:"error"`
	Files      int64  `json:"files"`
	Dirs       int64  `json:"dirs"`
	Bytes      int64  `json:"bytes"`
	Skipped    int64  `json:"skipped"`
	Current    string `json:"current"`
	ElapsedMS  int64  `json:"elapsed_ms"`
	DurationMS int64  `json:"duration_ms"`
}

// StartScan 启动一次扫描，立即返回；进度用 Progress() 轮询。
func (a *App) StartScan(disk string, deep bool) error {
	if !a.running.CompareAndSwap(false, true) {
		return errors.New("已有扫描在进行中")
	}

	roots := []string{}
	if disk != "" {
		roots = append(roots, disk+`:\\`)
	}

	ctx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	a.cancel = cancel
	a.result = nil
	a.started = time.Now()
	a.mu.Unlock()
	a.done.Store(false)
	a.lastErr.Store("")

	go func() {
		defer a.running.Store(false)
		defer cancel()

		opts := scan.Options{
			Roots:    roots,
			Deep:     deep,
			Progress: a.prog,
			TopFiles: 30,
		}
		res, err := scan.Run(ctx, opts)
		if err != nil {
			a.lastErr.Store(err.Error())
		}
		a.mu.Lock()
		a.result = res
		a.mu.Unlock()
		a.done.Store(true)

		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "scan:finished")
		}
	}()

	return nil
}

// CancelScan 请求取消当前扫描。
func (a *App) CancelScan() {
	a.mu.Lock()
	cancel := a.cancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Progress 返回当前进度快照。
func (a *App) Progress() ProgressDTO {
	s := a.prog.Snapshot()
	dto := ProgressDTO{
		Running:   a.running.Load(),
		Done:      a.done.Load(),
		Files:     s.Files,
		Dirs:      s.Dirs,
		Bytes:     s.Bytes,
		Skipped:   s.Skipped,
		Current:   s.Current,
		ElapsedMS: time.Since(a.started).Milliseconds(),
	}
	if e, ok := a.lastErr.Load().(string); ok {
		dto.Error = e
	}
	a.mu.Lock()
	if a.result != nil {
		dto.DurationMS = a.result.DurationMS
	}
	a.mu.Unlock()
	return dto
}

// Result 返回最近一次扫描的结果；尚未完成时返回 nil（前端据此判断）。
func (a *App) Result() *model.ScanResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.result
}

// Volumes 返回本机固定磁盘，供扫描页选择目标。
func (a *App) Volumes() []model.Volume {
	vols, err := sys.EnumerateVolumes()
	if err != nil {
		return nil
	}
	return vols
}

// Doctor 返回环境自检信息（扫描页展示）。
func (a *App) Doctor() []cli.Check {
	winapi.SetConsoleUTF8() // 对 GUI 进程无害，保持与 CLI 一致的编码前提
	return cli.RunDoctorChecks()
}

// ExportReport 把最近一次扫描导出为 HTML 报告并用浏览器打开。
func (a *App) ExportReport() (string, error) {
	a.mu.Lock()
	res := a.result
	a.mu.Unlock()
	if res == nil {
		return "", errors.New("还没有扫描结果，请先扫描")
	}

	out := "winclean-report.html"
	if err := report.WriteHTML(out, res, 500); err != nil {
		return "", err
	}
	abs, err := filepath.Abs(out)
	if err != nil {
		return "", err
	}
	if err := cli.OpenInBrowser(abs); err != nil {
		return abs, fmt.Errorf("已生成 %s，但自动打开失败: %w", abs, err)
	}
	return abs, nil
}
