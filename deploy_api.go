package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"winclean/internal/deploy"
	"winclean/internal/report"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// 部署打包模块的后端接口。
//
// 与扫描模块同样的原则：进入页面只做零成本初始化，
// 真正的活（探测工具链、跑 mvn/npm）都由用户显式触发。
// 打包在后台 goroutine 执行，事件通过 pack:event 推给界面。

const packLogLimit = 3000 // 日志环形缓冲行数

// packSession 保存当前（或最近一次）打包会话状态。
type packSession struct {
	mu      sync.Mutex
	running bool
	phase   string
	project string
	module  string
	percent int
	outputs []string
	errMsg  string
	logs    []string
	cancel  context.CancelFunc
}

// DeployDTO 是部署页需要的全部数据。
type DeployDTO struct {
	Config     deploy.Config       `json:"config"`
	Summary    string              `json:"summary"`
	KeepEnv    string              `json:"keep_env"`
	BaseExists bool                `json:"base_exists"`
	OutputOK   bool                `json:"output_ok"`
	PackBusy   bool                `json:"pack_busy"`
	MavenSet   string              `json:"maven_set"`
	JDKSet     string              `json:"jdk_set"`
}

// PackStatusDTO 是打包进度快照。
type PackStatusDTO struct {
	Running bool     `json:"running"`
	Phase   string   `json:"phase"`
	Project string   `json:"project"`
	Module  string   `json:"module"`
	Percent int      `json:"percent"`
	Outputs []string `json:"outputs"`
	Error   string   `json:"error"`
	Logs    []string `json:"logs"`
}

// deploySession 是进程级的打包会话（单窗口应用，无需多实例）。
var deploySession = &packSession{}

// ───────── 工具链探测 ─────────

// DetectToolchain 自动探测本机的 JDK 与 Maven。
//
// scanRoots 是额外的代码根目录（用于发现项目自带的 mvnw）。
// 为空时用部署配置里的 base_path。
func (a *App) DetectToolchain(scanRoots []string) deploy.DetectResult {
	roots := scanRoots
	if len(roots) == 0 {
		if bp := strings.TrimSpace(a.cfg.Deploy.BasePath); bp != "" {
			roots = []string{bp}
		}
	}
	return deploy.Detect(roots)
}

// ───────── 配置读写 ─────────

// GetDeploy 返回部署页数据。
func (a *App) GetDeploy() DeployDTO {
	cfg := a.cfg.Deploy
	base := strings.TrimSpace(cfg.BasePath)
	out := strings.TrimSpace(cfg.OutputDir)

	baseOK := false
	if base != "" {
		if st, err := os.Stat(base); err == nil && st.IsDir() {
			baseOK = true
		}
	}
	outOK := out != ""

	deploySession.mu.Lock()
	busy := deploySession.running
	deploySession.mu.Unlock()

	return DeployDTO{
		Config:     cfg,
		Summary:    cfg.Describe(),
		KeepEnv:    cfg.KeepEnv,
		BaseExists: baseOK,
		OutputOK:   outOK,
		PackBusy:   busy,
		MavenSet:   cfg.MavenPath,
		JDKSet:     cfg.JDKPath,
	}
}

// SaveDeploy 保存部署配置并持久化。
//
// 保存前先规整：把历史遗留的、写错的模块 Root 改写成正确值再落盘，
// 避免错误路径在配置里一直传下去。
func (a *App) SaveDeploy(cfg deploy.Config) error {
	deploy.NormalizeConfig(&cfg, cfg.BasePath)
	a.cfg.Deploy = cfg
	return a.cfg.Save()
}

// ImportDeployConfig 从既有工具（bundle-packer）的 config.yaml 导入配置。
//
// 导入后立即保存，省得用户再点一次保存。
func (a *App) ImportDeployConfig(path string) (DeployDTO, error) {
	cfg, err := deploy.ImportExternalConfig(path)
	if err != nil {
		return DeployDTO{}, err
	}
	a.cfg.Deploy = cfg
	if err := a.cfg.Save(); err != nil {
		return DeployDTO{}, fmt.Errorf("导入成功但保存失败: %w", err)
	}
	return a.GetDeploy(), nil
}

// DiscoverResult 是项目扫描结果。
//
// 为什么用一个结构体而不是 (projects, notes, error) 多返回值：
// Wails 绑定在有多返回值时**只把第一个值交给 JS**（除 error 会被转成 reject），
// 第二个返回值会被静默丢弃。前端写 `const [a, b] = await ...` 拿不到第二个值，
// 数组为空时还会抛「not iterable」——实测踩过。
// 凡是要一起返回给前端的数据，都必须装进一个结构体。
type DiscoverResult struct {
	Root     string                       `json:"root"`
	Projects []deploy.DiscoveredProject   `json:"projects"`
	Notes    []string                     `json:"notes"`
}

// DiscoverDeployProjects 扫描代码根目录，自动发现项目与模块。
//
// 这是「傻瓜式配置」的核心：用户只需选一个代码目录，
// 不必手抄项目与模块清单。扫描结果是启发式的，返回给用户确认，不直接生效。
func (a *App) DiscoverDeployProjects(codeRoot string) (DiscoverResult, error) {
	if strings.TrimSpace(codeRoot) == "" {
		codeRoot = a.cfg.Deploy.BasePath
	}
	projects, notes, err := deploy.DiscoverProjects(codeRoot, 3)
	if err != nil {
		return DiscoverResult{}, err
	}
	return DiscoverResult{Root: codeRoot, Projects: projects, Notes: notes}, nil
}

// AdoptDiscoveredProject 把扫描发现的某个项目写入配置（覆盖同名项目）。
//
// 单独一个方法而不是让前端整体重写配置：前端只传"用户确认过的这个项目"，
// 避免把未确认的扫描结果一并落盘。
func (a *App) AdoptDiscoveredProject(proj deploy.DiscoveredProject, outputDir string, keepEnv string) error {
	pc := deploy.ProjectConfig{Name: proj.Name, Root: proj.Root}
	for _, m := range proj.Modules {
		pc.Modules = append(pc.Modules, deploy.ModuleConfig{
			Name: m.Name, Type: m.Type, Root: m.Root,
			Script: m.Script, Source: m.Source, Output: m.Output,
		})
	}

	// 覆盖同名项目，其余保留
	replaced := false
	for i := range a.cfg.Deploy.Projects {
		if a.cfg.Deploy.Projects[i].Name == pc.Name {
			a.cfg.Deploy.Projects[i] = pc
			replaced = true
			break
		}
	}
	if !replaced {
		a.cfg.Deploy.Projects = append(a.cfg.Deploy.Projects, pc)
	}
	if strings.TrimSpace(outputDir) != "" {
		a.cfg.Deploy.OutputDir = strings.TrimSpace(outputDir)
	}
	if strings.TrimSpace(keepEnv) != "" {
		a.cfg.Deploy.KeepEnv = strings.TrimSpace(keepEnv)
	}
	return a.cfg.Save()
}

// RemoveDeployProject 从配置里移除一个项目。
func (a *App) RemoveDeployProject(name string) error {
	kept := a.cfg.Deploy.Projects[:0]
	for _, p := range a.cfg.Deploy.Projects {
		if p.Name != name {
			kept = append(kept, p)
		}
	}
	a.cfg.Deploy.Projects = kept
	return a.cfg.Save()
}

// PrecheckDeploy 做打包前预检（把失败拦在点击之前）。
func (a *App) PrecheckDeploy(project string, modules []string, outputDir string) (deploy.PrecheckResult, error) {
	return deploy.Precheck(a.cfg.Deploy, deploy.RunOptions{
		Project: project, Modules: modules, OutputDir: outputDir,
	}), nil
}

// BuildSpaceReport 从最近一次扫描结果构建人类可读的空间报告。
//
// 这是扫描结果的「理解层」：不展示原始路径与字节数，
// 而是按「你能做什么」分组——系统组件 / 已安装软件 / 开发工具与缓存 /
// 用户数据 / 可安全清理 / 没认出来的。
func (a *App) BuildSpaceReport() (report.SpaceReport, error) {
	a.mu.Lock()
	res := a.result
	a.mu.Unlock()
	if res == nil {
		return report.SpaceReport{}, errors.New("还没有扫描结果")
	}
	return report.BuildSpaceReport(res), nil
}

// BuildDecisionList 从最近一次扫描结果生成三色决策清单。
//
// 这是给用户看的最高层视图：
//   绿色区（可以删除）→ 默认全选
//   黄色区（需要你决定）→ 默认不选，用户逐个判断
//   红色区（不要动）→ 无勾选框，只展示
func (a *App) BuildDecisionList() ([]report.DecisionGroup, error) {
	a.mu.Lock()
	res := a.result
	a.mu.Unlock()
	if res == nil {
		return nil, errors.New("还没有扫描结果")
	}
	return report.BuildDecisionList(res), nil
}

// SuggestBasePaths 返回本机可能作为代码根目录的候选（配置页第 1 步的默认值）。
func (a *App) SuggestBasePaths() []string {
	var out []string
	candidates := []string{
		`D:\code`, `C:\code`, `D:\workspace`, `C:\workspace`,
		`D:\projects`, `D:\work`, `E:\code`,
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, "code"), filepath.Join(home, "IdeaProjects"),
			filepath.Join(home, "workspace"))
	}
	for _, c := range candidates {
		if dirExists(c) {
			out = append(out, c)
		}
	}
	// 已配置的放最前
	if bp := strings.TrimSpace(a.cfg.Deploy.BasePath); bp != "" && dirExists(bp) {
		out = append([]string{bp}, out...)
	}
	return dedupeStrings(out)
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// ───────── 目录选择 ─────────

// PickDirectory 弹出系统目录选择框。
//
// 这就是本次要改进的第二点：输出目录不再写死在配置里，
// 直接在界面选。current 作为对话框的初始位置。
func (a *App) PickDirectory(title, current string) (string, error) {
	if title == "" {
		title = "选择目录"
	}
	start := current
	if start == "" || !dirExists(start) {
		start = ""
	}
	dir, err := runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title:                title,
		DefaultDirectory:     start,
		CanCreateDirectories: true,
	})
	if err != nil {
		return "", err
	}
	return dir, nil // 用户取消时返回空串，不算错误
}

// OpenDirectory 在资源管理器中打开目录。
func (a *App) OpenDirectory(path string) error {
	if path == "" {
		return errors.New("路径为空")
	}
	if !dirExists(path) {
		return fmt.Errorf("目录不存在: %s", path)
	}
	return exec.Command("explorer.exe", path).Start()
}

// PickConfigFile 弹出文件选择框（用于导入既有打包工具的 config.yaml）。
func (a *App) PickConfigFile(title string) (string, error) {
	if title == "" {
		title = "选择 config.yaml"
	}
	f, err := runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title: title,
		Filters: []runtime.FileFilter{
			{DisplayName: "配置文件 (*.yaml;*.yml)", Pattern: "*.yaml;*.yml"},
			{DisplayName: "所有文件", Pattern: "*.*"},
		},
	})
	if err != nil {
		return "", err
	}
	return f, nil
}

// ───────── 打包 ─────────

// StartPack 启动一次打包（后台执行，进度与日志用事件推送）。
func (a *App) StartPack(project string, modules []string, outputDir string) error {
	deploySession.mu.Lock()
	if deploySession.running {
		deploySession.mu.Unlock()
		return errors.New("已有打包任务在进行中")
	}
	ctx, cancel := context.WithCancel(context.Background())
	deploySession.running = true
	deploySession.phase = "准备"
	deploySession.project = project
	deploySession.module = ""
	deploySession.percent = 0
	deploySession.outputs = nil
	deploySession.errMsg = ""
	deploySession.logs = nil
	deploySession.cancel = cancel
	deploySession.mu.Unlock()

	cfg := a.cfg.Deploy
	opts := deploy.RunOptions{Project: project, Modules: modules, OutputDir: outputDir}

	go func() {
		defer func() {
			deploySession.mu.Lock()
			deploySession.running = false
			deploySession.cancel = nil
			deploySession.mu.Unlock()
		}()

		err := deploy.Run(ctx, cfg, opts, func(ev deploy.Event) {
			deploySession.mu.Lock()
			switch ev.Kind {
			case "state":
				if ev.Phase != "" {
					deploySession.phase = ev.Phase
				}
				if ev.Project != "" {
					deploySession.project = ev.Project
				}
				deploySession.module = ev.Module
				deploySession.percent = ev.Percent
				if ev.Line != "" {
					deploySession.logs = appendLog(deploySession.logs, ev.Line)
				}
			case "log":
				if ev.Project != "" {
					deploySession.project = ev.Project
				}
				if ev.Module != "" {
					deploySession.module = ev.Module
				}
				deploySession.logs = appendLog(deploySession.logs, ev.Line)
			case "done":
				deploySession.outputs = ev.Outputs
				deploySession.percent = 100
				deploySession.logs = appendLog(deploySession.logs, ev.Line)
			}
			deploySession.mu.Unlock()

			if a.ctx != nil {
				runtime.EventsEmit(a.ctx, "pack:event", ev)
			}
		})

		if err != nil {
			deploySession.mu.Lock()
			deploySession.errMsg = err.Error()
			deploySession.logs = appendLog(deploySession.logs, "❌ "+err.Error())
			deploySession.mu.Unlock()
		}
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "pack:finished", err != nil)
		}
	}()

	return nil
}

// CancelPack 取消当前打包（终止整棵子进程树）。
func (a *App) CancelPack() {
	deploySession.mu.Lock()
	cancel := deploySession.cancel
	deploySession.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// PackStatus 返回打包进度快照（含最近日志，便于切换页面后恢复显示）。
func (a *App) PackStatus() PackStatusDTO {
	deploySession.mu.Lock()
	defer deploySession.mu.Unlock()
	logs := make([]string, len(deploySession.logs))
	copy(logs, deploySession.logs)
	outputs := make([]string, len(deploySession.outputs))
	copy(outputs, deploySession.outputs)

	return PackStatusDTO{
		Running: deploySession.running,
		Phase:   deploySession.phase,
		Project: deploySession.project,
		Module:  deploySession.module,
		Percent: deploySession.percent,
		Outputs: outputs,
		Error:   deploySession.errMsg,
		Logs:    logs,
	}
}

// appendLog 追加日志并限制缓冲上限（避免长时间构建把内存撑爆）。
func appendLog(logs []string, line string) []string {
	if line == "" {
		return logs
	}
	// mvn 输出里可能有 \r 刷新的进度行，拆开成多行更易读
	for _, l := range strings.Split(strings.ReplaceAll(line, "\r", "\n"), "\n") {
		l = strings.TrimRight(l, " \t")
		if l == "" {
			continue
		}
		logs = append(logs, l)
	}
	if len(logs) > packLogLimit {
		logs = logs[len(logs)-packLogLimit:]
	}
	return logs
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// 保留：为将来支持「记住上次选择的输出目录」预留（当前由前端 localStorage 承担）。
var _ = filepath.Join
var _ atomic.Bool
