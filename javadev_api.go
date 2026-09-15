package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"winclean/internal/deploy"
	"winclean/internal/javadev"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// 「Java 开发」模块的后端接口。
//
// 与部署打包同源：项目与模块清单复用 deploy 配置（用户不必配两遍），
// 在此基础上补齐开发态需要的三件事——把模块跑起来（服务启停）、
// 把依赖拉齐（加载依赖）、把改动看清（Git 变更）。
//
// 边界刻意收窄：不改源码、不改 git 历史、不做代码分析。
// 这是一个「运行栏 + 状态看板」，不是一个 IDE。

// 服务运行时按需创建。
//
// 刻意不写成包级 var devMgr = javadev.NewManager()：按项目规则（AGENTS.md §2.1 第 5 条），
// 模块状态不应在包初始化时建立，否则「从没进过这个页面的用户也在为它付成本」。
// 改成首次真正用到时才建——而首次用到必然来自用户进入模块页后的显式动作。
var (
	devMgrOnce sync.Once
	devMgrInst *javadev.Manager
)

func devMgr() *javadev.Manager {
	devMgrOnce.Do(func() { devMgrInst = javadev.NewManager() })
	return devMgrInst
}

// toolchainCache 缓存 JDK / Maven 探测结果。
//
// 为什么需要：服务列表每次刷新都要知道 JDK 与 Maven 在哪（构造子进程环境用），
// 而探测要扫多个目录，放在刷新路径上会拖慢界面。配置里显式写了就用配置的值，
// 只有留空时才探测，且探测结果缓存 5 分钟。
var toolchainCache struct {
	mu  sync.Mutex
	at  time.Time
	jdk string
	mvn string
}

func (a *App) resolveToolchain() (jdk, mvn string) {
	cfg := a.cfg.Deploy
	jdk = strings.TrimSpace(cfg.JDKPath)
	mvn = strings.TrimSpace(cfg.MavenPath)
	if jdk != "" && mvn != "" {
		return jdk, mvn
	}

	toolchainCache.mu.Lock()
	defer toolchainCache.mu.Unlock()
	if time.Since(toolchainCache.at) > 5*time.Minute {
		d := deploy.Detect(nil)
		toolchainCache.jdk, toolchainCache.mvn = "", ""
		if len(d.JDKs) > 0 && d.JDKs[0].Valid {
			toolchainCache.jdk = d.JDKs[0].Path
		}
		if len(d.Mavens) > 0 && d.Mavens[0].Valid {
			toolchainCache.mvn = d.Mavens[0].Path
		}
		toolchainCache.at = time.Now()
	}
	if jdk == "" {
		jdk = toolchainCache.jdk
	}
	if mvn == "" {
		mvn = toolchainCache.mvn
	}
	return jdk, mvn
}

// ───────── DTO ─────────

// DevServiceDTO 是一个服务在界面上的完整视图。
type DevServiceDTO struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Project string `json:"project"`
	Module  string `json:"module"`
	Type    string `json:"type"`
	Dir     string `json:"dir"`
	Run     string `json:"run"`
	Deps    string `json:"deps"`
	Port    int    `json:"port"`
	Ready   bool   `json:"ready"`
	Reason  string `json:"reason,omitempty"`

	State     string `json:"state"`
	Task      string `json:"task,omitempty"`
	PID       int    `json:"pid"`
	UptimeMS  int64  `json:"uptime_ms"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Error     string `json:"error,omitempty"`
	StartedAt string `json:"started_at,omitempty"`

	// Logs 只是最近若干行，用于刷新后仍在卡片上看到上下文；
	// 完整日志走 DevServiceLogs（避免每次轮询都传几千行）。
	Logs []string `json:"logs"`

	// PortStatus 是该服务端口的占用情况（一次刷新只读一次系统 TCP 表）
	PortStatus *DevPortStatusDTO `json:"port_status,omitempty"`
}

// DevPageDTO 是「Java 开发」页需要的全部数据。
type DevPageDTO struct {
	BasePath string          `json:"base_path"`
	Summary  string          `json:"summary"`
	Services []DevServiceDTO `json:"services"`
	Notes    []string        `json:"notes"`
	Git      []DevRepoDTO    `json:"git"`
	JDK      string          `json:"jdk"`
	Maven    string          `json:"maven"`
}

// ───────── 绑定边界上的 DTO ─────────
//
// 所有跨越 Wails 绑定的类型都定义在 main 包，不直接用 internal/javadev 的类型。
// 原因见 app.go 里那段记录：绑定方法的签名一旦引用别的包的类型，
// Wails 的运行时绑定会静默丢弃整个方法（前端调用时报 "is not a function"），
// 而且没有任何报错提示。多写一层转换，换掉一整类「莫名其妙不生效」的问题。

// DevHolderDTO 是占用端口的进程。
type DevHolderDTO struct {
	PID     uint32 `json:"pid"`
	Process string `json:"process"`
	Path    string `json:"path"`
	Addr    string `json:"local_addr"`
	State   string `json:"state"`
	Safe    bool   `json:"safe"`
	Reason  string `json:"reason,omitempty"`
}

// DevPortStatusDTO 是一个端口的占用情况。
type DevPortStatusDTO struct {
	Port      int            `json:"port"`
	Busy      bool           `json:"busy"`
	Holders   []DevHolderDTO `json:"holders"`
	Listeners int            `json:"listeners"`
}

// DevKillResultDTO 是一次「杀端口」的结果。
type DevKillResultDTO struct {
	Port   int              `json:"port"`
	Killed []DevHolderDTO   `json:"killed"`
	Failed []string         `json:"failed"`
	Status DevPortStatusDTO `json:"status"`
}

// DevChangeDTO 是一个改动文件。
type DevChangeDTO struct {
	Code    string `json:"code"`
	Path    string `json:"path"`
	Kind    string `json:"kind"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	Binary  bool   `json:"binary"`
}

// DevRepoDTO 是一个项目的仓库状态。
type DevRepoDTO struct {
	Project   string         `json:"project"`
	Dir       string         `json:"dir"`
	IsRepo    bool           `json:"is_repo"`
	Branch    string         `json:"branch"`
	Commit    string         `json:"commit"`
	Changes   []DevChangeDTO `json:"changes"`
	Truncated bool           `json:"truncated"`
	Clean     bool           `json:"clean"`
	Err       string         `json:"err"`
}

// DevHintDTO 是单个模块的启动方式识别结果。
type DevHintDTO struct {
	Run   string `json:"run"`
	Port  int    `json:"port"`
	Note  string `json:"note"`
	Found bool   `json:"found"`
}

func toHolderDTO(h javadev.Holder) DevHolderDTO {
	return DevHolderDTO{
		PID: h.PID, Process: h.Process, Path: h.Path, Addr: h.Addr,
		State: h.State, Safe: h.Safe, Reason: h.Reason,
	}
}

func toPortStatusDTO(st javadev.PortStatus) DevPortStatusDTO {
	out := DevPortStatusDTO{Port: st.Port, Busy: st.Busy, Listeners: st.Listeners}
	for _, h := range st.Holders {
		out.Holders = append(out.Holders, toHolderDTO(h))
	}
	return out
}

func toKillResultDTO(res javadev.KillResult) DevKillResultDTO {
	out := DevKillResultDTO{Port: res.Port, Failed: res.Failed, Status: toPortStatusDTO(res.Status)}
	for _, h := range res.Killed {
		out.Killed = append(out.Killed, toHolderDTO(h))
	}
	return out
}

func toRepoDTO(r javadev.Repo) DevRepoDTO {
	out := DevRepoDTO{
		Project: r.Project, Dir: r.Dir, IsRepo: r.IsRepo, Branch: r.Branch,
		Commit: r.Commit, Truncated: r.Truncated, Clean: r.Clean, Err: r.Err,
	}
	for _, c := range r.Changes {
		out.Changes = append(out.Changes, DevChangeDTO{
			Code: c.Code, Path: c.Path, Kind: c.Kind,
			Added: c.Added, Deleted: c.Deleted, Binary: c.Binary,
		})
	}
	return out
}

// devLogTail 是随轮询返回的日志尾行数。
const devLogTail = 80

// ───────── 服务清单 ─────────

// buildDevSpecs 从配置生成服务清单。
func (a *App) buildDevSpecs() ([]javadev.Spec, []string) {
	cfg := a.cfg.Deploy
	base := strings.TrimSpace(cfg.BasePath)
	var notes []string
	if base == "" {
		return nil, []string{"还没有选择代码根目录，请到「配置」页完成三步设置"}
	}
	if !dirExists(base) {
		return nil, []string{"代码根目录不存在：" + base}
	}
	if len(cfg.Projects) == 0 {
		return nil, []string{"还没有配置任何项目，请到「配置」页扫描代码目录"}
	}

	jdk, mvn := a.resolveToolchain()
	if jdk == "" {
		notes = append(notes, "没有探测到 JDK：需要运行 Java 模块时请到「配置」页指定")
	}

	var specs []javadev.Spec
	for _, proj := range cfg.Projects {
		for _, mod := range proj.Modules {
			dir := deploy.ResolveModuleDir(base, proj, mod)
			typ := mod.Type
			if typ == "" {
				typ = "backend"
			}
			run := strings.TrimSpace(mod.Run)
			spec := javadev.Spec{
				ID:        serviceID(proj.Name, mod.Name),
				Name:      mod.Name,
				Project:   proj.Name,
				Module:    mod.Name,
				Type:      typ,
				Dir:       dir,
				Run:       run,
				Deps:      deploy.DepsCommand(dir, typ),
				Port:      mod.Port,
				JDK:       jdk,
				MavenPath: mvn,
			}
			switch {
			case !dirExists(dir):
				spec.Ready = false
				spec.Reason = "模块目录不存在：" + dir
			case run == "":
				spec.Ready = false
				spec.Reason = "还没有启动命令，可点「识别」自动填，或手填后保存"
			default:
				spec.Ready = true
			}
			specs = append(specs, spec)
		}
	}
	return specs, notes
}

func serviceID(project, module string) string { return project + "/" + module }

// GetDevPage 返回开发页数据（服务状态 + 端口占用，withGit 时附带 Git 变更）。
//
// withGit 单独成参是有意的：git 查询要起多个进程（每项目 3~4 条命令），
// 而服务状态轮询每几秒一次。把两者绑在一起，轮询就会变成持续无谓的开销；
// 前端只在用户切到「变更」页签时才带 true。
//
// 每次调用都会重新读配置里的项目清单（用户可能在配置页改过），
// 然后一次性读系统 TCP 表，把每个端口的占用情况分派下去。
func (a *App) GetDevPage(withGit bool) DevPageDTO {
	specs, notes := a.buildDevSpecs()
	views := devMgr().Init(specs)

	viewByID := make(map[string]javadev.View, len(views))
	for _, v := range views {
		viewByID[v.ID] = v
	}

	ports := make([]int, 0, len(specs))
	for _, s := range specs {
		if s.Port > 0 {
			ports = append(ports, s.Port)
		}
	}
	portStatus := javadev.PortStatusBatch(ports)

	out := DevPageDTO{
		BasePath: strings.TrimSpace(a.cfg.Deploy.BasePath),
		Summary:  a.cfg.Deploy.Describe(),
		Notes:    notes,
	}
	jdk, mvn := a.resolveToolchain()
	out.JDK, out.Maven = jdk, mvn

	for _, s := range specs {
		dto := DevServiceDTO{
			ID: s.ID, Name: s.Name, Project: s.Project, Module: s.Module,
			Type: s.Type, Dir: s.Dir, Run: s.Run, Deps: s.Deps, Port: s.Port,
			Ready: s.Ready, Reason: s.Reason,
			State: javadev.StateStopped,
		}
		if v, ok := viewByID[s.ID]; ok {
			dto.State = v.State
			dto.Task = v.Task
			dto.PID = v.PID
			dto.UptimeMS = v.UptimeMS
			dto.ExitCode = v.ExitCode
			dto.Error = v.Error
			if v.StartedAt != nil {
				dto.StartedAt = v.StartedAt.Format("15:04:05")
			}
		}
		logs := devMgr().Logs(s.ID)
		if len(logs) > devLogTail {
			logs = logs[len(logs)-devLogTail:]
		}
		dto.Logs = logs
		if s.Port > 0 {
			if st, ok := portStatus[s.Port]; ok {
				conv := toPortStatusDTO(st)
				dto.PortStatus = &conv
			}
		}
		out.Services = append(out.Services, dto)
	}

	out.Git = nil
	if withGit {
		out.Git = a.gitRepos()
	}
	return out
}

// gitRepos 返回各项目的 git 状态。
//
// 只读、按需——用户切到「变更」页签时才调用（见 GitChanges）。
func (a *App) gitRepos() []DevRepoDTO {
	cfg := a.cfg.Deploy
	base := strings.TrimSpace(cfg.BasePath)
	if base == "" || len(cfg.Projects) == 0 {
		return nil
	}
	dirs := map[string]string{}
	for _, p := range cfg.Projects {
		dir := filepath.Join(base, filepath.FromSlash(strings.TrimSpace(p.Root)))
		if dirExists(dir) {
			dirs[p.Name] = dir
		}
	}
	if len(dirs) == 0 {
		return nil
	}
	repos := javadev.GitStatusAll(dirs)
	out := make([]DevRepoDTO, 0, len(repos))
	for _, r := range repos {
		out = append(out, toRepoDTO(r))
	}
	return out
}

// GitChanges 查询 git 变更（「变更」页签刷新时调用）。
func (a *App) GitChanges() []DevRepoDTO { return a.gitRepos() }

func (a *App) serviceSpec(id string) (javadev.Spec, error) {
	specs, _ := a.buildDevSpecs()
	for _, s := range specs {
		if s.ID == id {
			return s, nil
		}
	}
	return javadev.Spec{}, fmt.Errorf("未知的服务: %s", id)
}

// ───────── 启停 ─────────

// StartDevService 启动一个服务（后台运行，日志用 dev:event 推送）。
func (a *App) StartDevService(id string) error {
	spec, err := a.serviceSpec(id)
	if err != nil {
		return err
	}
	if !spec.Ready {
		return errors.New(spec.Reason)
	}

	// 启动前先看一眼端口：被占了就直说是谁占的，
	// 而不是等 mvn 跑完再抛一句「端口已被占用」让用户自己查。
	if spec.Port > 0 {
		if st := javadev.PortStatusOf(spec.Port); st.Listeners > 0 {
			return fmt.Errorf("端口 %d 已被占用（%s，PID %d）。可在端口卡片上点「杀端口」后再启动",
				spec.Port, st.Holders[0].Process, st.Holders[0].PID)
		}
	}

	devMgr().Init([]javadev.Spec{spec})
	return devMgr().Start(spec.ID)
}

// StopDevService 停止服务（同步等待进程树退出）。
func (a *App) StopDevService(id string) error { return devMgr().Stop(id) }

// RestartDevService 重启服务。
func (a *App) RestartDevService(id string) error { return devMgr().Restart(id) }

// LoadDevDeps 加载该模块的依赖。
func (a *App) LoadDevDeps(id string) error {
	spec, err := a.serviceSpec(id)
	if err != nil {
		return err
	}
	if !dirExists(spec.Dir) {
		return fmt.Errorf("模块目录不存在：%s", spec.Dir)
	}
	devMgr().Init([]javadev.Spec{spec})
	return devMgr().LoadDeps(spec.ID)
}

// DevServiceLogs 返回某个服务的完整日志。要重启服务（清缓冲）请用 DevClearLogs。
func (a *App) DevServiceLogs(id string) []string { return devMgr().Logs(id) }

// DevClearLogs 清空某个服务的日志缓冲。
func (a *App) DevClearLogs(id string) {
	devMgr().ClearLogs(id)
}

// ───────── 端口 ─────────

// CheckPort 查询任意端口的占用情况（不限于配置里的端口）。
func (a *App) CheckPort(port int) (DevPortStatusDTO, error) {
	if port <= 0 || port > 65535 {
		return DevPortStatusDTO{}, fmt.Errorf("端口号不合法: %d", port)
	}
	return toPortStatusDTO(javadev.PortStatusOf(port)), nil
}

// KillPort 终止占用该端口的进程。
//
// 系统关键进程（svchost、lsass、explorer 等）一律拒绝，见 javadev.KillPort。
func (a *App) KillPort(port int) (DevKillResultDTO, error) {
	if port <= 0 || port > 65535 {
		return DevKillResultDTO{}, fmt.Errorf("端口号不合法: %d", port)
	}
	return toKillResultDTO(javadev.KillPort(port)), nil
}

// ───────── 编辑与识别 ─────────

// SaveDevService 保存某个模块的启动命令与端口。
func (a *App) SaveDevService(project, module, run string, port int) error {
	if port < 0 || port > 65535 {
		return fmt.Errorf("端口号不合法: %d", port)
	}
	cfg := &a.cfg.Deploy
	for pi := range cfg.Projects {
		if cfg.Projects[pi].Name != project {
			continue
		}
		for mi := range cfg.Projects[pi].Modules {
			mod := &cfg.Projects[pi].Modules[mi]
			if mod.Name != module {
				continue
			}
			mod.Run = strings.TrimSpace(run)
			mod.Port = port
			if err := a.cfg.Save(); err != nil {
				return err
			}
			// 立刻把新命令灌进运行时（否则要点两次才生效）
			devMgr().Init(mustSpecs(a))
			return nil
		}
		return fmt.Errorf("项目 %s 下没有模块 %s", project, module)
	}
	return fmt.Errorf("配置里没有项目 %s", project)
}

func mustSpecs(a *App) []javadev.Spec {
	specs, _ := a.buildDevSpecs()
	return specs
}

// DetectDevService 识别单个模块的启动命令与端口（只返回建议，不落盘）。
func (a *App) DetectDevService(project, module string) (DevHintDTO, error) {
	h, err := deploy.DevHintFor(a.cfg.Deploy, project, module)
	if err != nil {
		return DevHintDTO{}, err
	}
	return DevHintDTO{Run: h.Run, Port: h.Port, Note: h.Note, Found: h.Found}, nil
}

// DetectAllDevServices 给所有缺启动命令/端口的模块做一次识别并写入配置。
//
// 显式动作：不做启动时自动填充——用户如果把命令清空（例如他习惯用 IDEA 跑），
// 启动时又给填回来，就是程序在跟用户较劲。
func (a *App) DetectAllDevServices() (DetectResult, error) {
	filled := deploy.FillDevDefaults(&a.cfg.Deploy, a.cfg.Deploy.BasePath)
	if filled == 0 {
		return DetectResult{Filled: 0, Message: "没有需要补全的模块（或都没识别出来）"}, nil
	}
	if err := a.cfg.Save(); err != nil {
		return DetectResult{}, fmt.Errorf("识别成功但保存失败: %w", err)
	}
	return DetectResult{Filled: filled,
		Message: fmt.Sprintf("已补全 %d 项启动配置，可在卡片上核对或修改", filled)}, nil
}

// DetectResult 是批量识别结果。
type DetectResult struct {
	Filled  int    `json:"filled"`
	Message string `json:"message"`
}

// OpenProjectDir 在资源管理器中打开模块目录。
func (a *App) OpenProjectDir(dir string) error {
	if dir == "" || !dirExists(dir) {
		return fmt.Errorf("目录不存在: %s", dir)
	}
	return exec.Command("explorer.exe", dir).Start()
}

// ───────── 启动/退出钩子 ─────────

// initDevEvents 把运行时事件接到 Wails 事件总线上。
func (a *App) initDevEvents() {
	devMgr().SetEmitter(func(ev javadev.Event) {
		if a.ctx == nil {
			return
		}
		runtime.EventsEmit(a.ctx, "dev:event", ev)
	})
}

// shutdownDev 退出时停止由本程序启动的服务。
//
// 不做这件事的后果很具体：用户关掉助手后 java 进程还占着 8080，
// 下次开发时被「端口已被占用」绊住，而他完全不记得是谁占的。
func (a *App) shutdownDev(_ context.Context) {
	devMgr().StopAll()
}
