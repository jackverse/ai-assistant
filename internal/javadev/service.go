package javadev

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// 服务运行时：把「一个模块 + 一条命令 + 一个工作目录」变成一个可启停的后台进程。
//
// 与打包模块的关键差别：打包是一次性任务（跑完就结束），服务是长期进程
// （可能跑几小时）。因此这里的状态是持续持有的，日志是环形缓冲，
// 停止要等待真正退出，启动后还要能回答「它到底有没有把端口监听起来」。

const logLimit = 2000 // 每个实例保留的日志行数

// 服务状态。
const (
	StateStopped  = "stopped"  // 未运行
	StateStarting = "starting" // 已发起启动，尚未确认
	StateRunning  = "running"  // 进程在跑
	StateStopping = "stopping" // 正在终止
	StateExited   = "exited"   // 已退出（可能是失败退出，看 exit_code）
)

// Spec 是一个可启动服务的定义。
type Spec struct {
	ID      string `json:"id"` // project/module，全局唯一
	Name    string `json:"name"`
	Project string `json:"project"`
	Module  string `json:"module"`
	Type    string `json:"type"` // backend | frontend
	Dir     string `json:"dir"`  // 工作目录（模块根）
	Run     string `json:"run"`  // 启动命令
	Deps    string `json:"deps"` // 加载依赖命令
	Port    int    `json:"port"`
	// JDK / MavenPath 来自配置与自动探测，只用于构造子进程环境，不发给界面
	JDK       string `json:"-"`
	MavenPath string `json:"-"`
	// Ready 为 false 表示这个模块没法启动（缺启动命令或目录不存在）
	Ready  bool   `json:"ready"`
	Reason string `json:"reason,omitempty"`
}

// Task 是一次性任务（加载依赖）。
const TaskDeps = "deps"

// Event 是推给界面的运行时事件。
type Event struct {
	Kind     string `json:"kind"` // log | state
	ID       string `json:"id"`
	Line     string `json:"line,omitempty"`
	State    string `json:"state,omitempty"`
	Task     string `json:"task,omitempty"`
	PID      int    `json:"pid,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
}

// View 是运行时快照（不含日志，日志由 Logs 单独取）。
type View struct {
	ID        string     `json:"id"`
	State     string     `json:"state"`
	Task      string     `json:"task,omitempty"`
	PID       int        `json:"pid"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	StoppedAt *time.Time `json:"stopped_at,omitempty"`
	ExitCode  *int       `json:"exit_code,omitempty"`
	Error     string     `json:"error,omitempty"`
	UptimeMS  int64      `json:"uptime_ms"`
	LogLines  int        `json:"log_lines"`
}

// instance 是一个服务的运行时。
type instance struct {
	spec  Spec
	task  string // 空 = 服务进程；deps = 依赖任务
	state string
	pid   int
	start time.Time
	stop  time.Time
	code  *int
	err   string
	logs  []string

	cancel context.CancelFunc
	done   chan struct{}
}

// Manager 管理全部服务的运行时。
//
// 单窗口应用，一个 Manager 实例足够；所有可变状态由一把锁保护——
// 事件回调在锁外触发，避免「回调里再调 Manager」时自锁。
type Manager struct {
	mu        sync.Mutex
	instances map[string]*instance
	emitFn    func(Event)
}

func NewManager() *Manager {
	return &Manager{instances: map[string]*instance{}}
}

// SetEmitter 注入事件回调（main 包把它接到 Wails 事件上）。
func (m *Manager) SetEmitter(fn func(Event)) {
	m.mu.Lock()
	m.emitFn = fn
	m.mu.Unlock()
}

func (m *Manager) emit(ev Event) {
	m.mu.Lock()
	fn := m.emitFn
	m.mu.Unlock()
	if fn != nil {
		fn(ev)
	}
}

// get 取实例（不存在则按 spec 建一个）。
func (m *Manager) get(spec Spec) *instance {
	if inst, ok := m.instances[spec.ID]; ok {
		inst.spec = spec // 命令/端口可能刚被用户改过
		return inst
	}
	inst := &instance{spec: spec, state: StateStopped}
	m.instances[spec.ID] = inst
	return inst
}

// Init 注册服务清单（进入页面时调用一次），返回每个服务的运行时视图。
//
// 已不在配置里的实例会被清掉——但正在运行的不清（用户可能刚删了配置项，
// 进程还在跑，这时把状态抹掉会让他再也停不掉它）。
func (m *Manager) Init(specs []Spec) []View {
	m.mu.Lock()
	keep := map[string]bool{}
	for _, s := range specs {
		keep[s.ID] = true
		m.get(s)
	}
	for id, inst := range m.instances {
		if keep[id] {
			continue
		}
		if inst.state == StateRunning || inst.state == StateStarting || inst.state == StateStopping {
			continue
		}
		delete(m.instances, id)
	}
	views := m.viewsLocked()
	m.mu.Unlock()
	return views
}

// Views 返回全部实例的运行时快照。
func (m *Manager) Views() []View {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.viewsLocked()
}

func (m *Manager) viewsLocked() []View {
	out := make([]View, 0, len(m.instances))
	for _, inst := range m.instances {
		out = append(out, inst.view())
	}
	return out
}

func (inst *instance) view() View {
	v := View{
		ID:       inst.spec.ID,
		State:    inst.state,
		Task:     inst.task,
		PID:      inst.pid,
		ExitCode: inst.code,
		Error:    inst.err,
		LogLines: len(inst.logs),
	}
	if !inst.start.IsZero() {
		t := inst.start
		v.StartedAt = &t
	}
	if !inst.stop.IsZero() {
		t := inst.stop
		v.StoppedAt = &t
	}
	if inst.state == StateRunning && !inst.start.IsZero() {
		v.UptimeMS = time.Since(inst.start).Milliseconds()
	}
	return v
}

// Logs 返回某个实例的日志。
func (m *Manager) Logs(id string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst, ok := m.instances[id]
	if !ok {
		return nil
	}
	out := make([]string, len(inst.logs))
	copy(out, inst.logs)
	return out
}

// ClearLogs 清空某个实例的日志。
func (m *Manager) ClearLogs(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if inst, ok := m.instances[id]; ok {
		inst.logs = nil
	}
}

// Start 启动服务。
func (m *Manager) Start(id string) error {
	return m.launch(id, "")
}

// LoadDeps 执行依赖加载任务。
func (m *Manager) LoadDeps(id string) error {
	return m.launch(id, TaskDeps)
}

// launch 是启动/跑任务的公共路径。
func (m *Manager) launch(id, task string) error {
	m.mu.Lock()
	inst, ok := m.instances[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("未知的服务: %s", id)
	}
	if inst.isBusy() {
		m.mu.Unlock()
		return fmt.Errorf("%s 正在运行中，请先停止", inst.spec.Name)
	}

	cmdline := inst.spec.Run
	label := "启动服务"
	if task == TaskDeps {
		cmdline = inst.spec.Deps
		label = "加载依赖"
	}
	if strings.TrimSpace(cmdline) == "" {
		m.mu.Unlock()
		if task == TaskDeps {
			return fmt.Errorf("%s 没有可用的依赖命令", inst.spec.Name)
		}
		return fmt.Errorf("%s 还没有设置启动命令", inst.spec.Name)
	}
	if inst.spec.Dir == "" {
		m.mu.Unlock()
		return fmt.Errorf("%s 的模块目录未知", inst.spec.Name)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "cmd.exe")
	// 与打包模块同样的做法：用 CmdLine 直接接管命令行，
	// 绕开 Go 在 Windows 上对参数的二次转义（会把引号变成 \"，cmd.exe 不认）。
	cmd.SysProcAttr = procAttr("chcp 65001 >nul && " + cmdline)
	cmd.Dir = inst.spec.Dir
	cmd.Env = childEnv(inst.spec.JDK, inst.spec.MavenPath)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		m.mu.Unlock()
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		m.mu.Unlock()
		return err
	}

	inst.state = StateStarting
	inst.task = task
	inst.err = ""
	inst.code = nil
	inst.cancel = cancel
	inst.done = make(chan struct{})
	inst.logs = nil
	inst.pushLog(fmt.Sprintf("$ %s   (工作目录 %s)", cmdline, inst.spec.Dir))

	if err := cmd.Start(); err != nil {
		inst.state = StateStopped
		inst.err = err.Error()
		inst.pushLog("❌ " + label + "失败：" + err.Error())
		inst.cancel = nil
		close(inst.done)
		cancel()
		m.mu.Unlock()
		return fmt.Errorf("%s失败: %w", label, err)
	}

	inst.pid = cmd.Process.Pid
	inst.state = StateRunning
	inst.start = time.Now()
	inst.stop = time.Time{}
	view := inst.view()
	m.mu.Unlock()

	m.emit(Event{Kind: "state", ID: id, State: view.State, Task: task, PID: view.PID})

	// 输出流：两个管道各自读到 EOF 即结束
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		streamLines(stdout, func(line string) { m.pushLog(id, line) })
	}()
	go func() {
		defer wg.Done()
		streamLines(stderr, func(line string) { m.pushLog(id, line) })
	}()

	go func() {
		waitExit(ctx, cmd, func(code int, waitErr error) {
			wg.Wait()

			m.mu.Lock()
			inst.state = StateExited
			inst.pid = 0
			inst.stop = time.Now()
			c := code
			inst.code = &c
			inst.cancel = nil
			if waitErr != nil && ctx.Err() == nil {
				inst.err = waitErr.Error()
				inst.pushLog("❌ 进程异常退出：" + waitErr.Error())
			} else if code == 0 {
				inst.pushLog("■ 进程已退出（退出码 0）")
			} else {
				inst.pushLog(fmt.Sprintf("■ 进程已退出（退出码 %d）", code))
			}
			v := inst.view()
			close(inst.done)
			m.mu.Unlock()

			m.emit(Event{Kind: "state", ID: id, State: v.State, Task: task,
				ExitCode: v.ExitCode, Error: v.Error})
		})
	}()

	return nil
}

// Stop 停止服务（或中断依赖任务）。
//
// 同步等待进程真正退出（最多 15 秒），因为界面上「停止」按钮点完就该看到
// 状态变了；若只是发个 taskkill 就返回，用户会以为没生效而重复点。
func (m *Manager) Stop(id string) error {
	m.mu.Lock()
	inst, ok := m.instances[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("未知的服务: %s", id)
	}
	if !inst.isBusy() {
		m.mu.Unlock()
		return fmt.Errorf("%s 当前没有在运行", inst.spec.Name)
	}
	pid := inst.pid
	done := inst.done
	inst.state = StateStopping
	inst.pushLog("… 正在停止（终止整棵进程树）")
	m.mu.Unlock()

	m.emit(Event{Kind: "state", ID: id, State: StateStopping})

	if pid > 0 {
		killTree(pid)
	}
	// 兜底：树杀失败（或 pid 丢失）时按取消上下文处理
	if done != nil {
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			m.mu.Lock()
			if inst.cancel != nil {
				inst.cancel()
			}
			m.mu.Unlock()
			<-done
		}
	}
	return nil
}

// Restart 重启服务（先停后启）。
func (m *Manager) Restart(id string) error {
	m.mu.Lock()
	inst, ok := m.instances[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("未知的服务: %s", id)
	}
	busy := inst.isBusy()
	m.mu.Unlock()

	if busy {
		if err := m.Stop(id); err != nil {
			return err
		}
	}
	return m.Start(id)
}

// StopAll 停止全部在跑的服务（程序退出时调用）。
//
// 不这么做的话，用户关掉助手后 java 进程还占着端口，下次开发就会被
// 「端口已被占用」绊住，而他根本不记得是谁占的。
func (m *Manager) StopAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.instances))
	for id, inst := range m.instances {
		if inst.isBusy() {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		_ = m.Stop(id)
	}
}

func (inst *instance) isBusy() bool {
	switch inst.state {
	case StateRunning, StateStarting, StateStopping:
		return true
	}
	return false
}

// pushLog 追加日志（调用方需持锁）。
func (inst *instance) pushLog(line string) {
	if line == "" {
		return
	}
	inst.logs = appendLogLine(inst.logs, line, logLimit)
}

// pushLog 追加日志并推送事件。
func (m *Manager) pushLog(id, line string) {
	m.mu.Lock()
	inst, ok := m.instances[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	inst.pushLog(line)
	m.mu.Unlock()

	m.emit(Event{Kind: "log", ID: id, Line: line})
}
