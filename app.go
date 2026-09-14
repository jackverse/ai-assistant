package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"winclean/internal/cli"
	"winclean/internal/model"
	"winclean/internal/report"
	"winclean/internal/scan"
	"winclean/internal/sys"
	"winclean/internal/winapi"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// App 是暴露给前端的后端对象。
//
// 设计原则：前端只通过这里的少量方法交互，所有真正的逻辑都在
// internal/* 里——GUI 只是同一套引擎的另一个入口，与 CLI 完全等价。
type App struct {
	ctx context.Context

	mu      sync.Mutex
	cancel  context.CancelFunc
	prog    *scan.Progress
	result  *model.ScanResult
	running atomic.Bool
	done    atomic.Bool
	lastErr atomic.Value // string
	started time.Time
}

// NewApp 创建应用对象。
func NewApp() *App {
	return &App{prog: scan.NewProgress()}
}

// startup 由 Wails 在窗口就绪后调用，保存上下文供事件推送使用。
func (a *App) startup(ctx context.Context) { a.ctx = ctx }

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

// Volumes 返回本机固定磁盘，供启动时选择扫描目标。
func (a *App) Volumes() []model.Volume {
	vols, err := sys.EnumerateVolumes()
	if err != nil {
		return nil
	}
	return vols
}

// Doctor 返回环境自检信息。
func (a *App) Doctor() []cli.Check {
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

// EnvInfo 返回界面顶部展示的环境信息。
func (a *App) EnvInfo() map[string]string {
	elevated := sys.IsElevated()
	s := "普通用户"
	if elevated {
		s = "管理员"
	}
	winapi.SetConsoleUTF8() // 对 GUI 进程无害，保持与 CLI 一致的编码前提
	return map[string]string{
		"elevated": s,
		"note":     "普通用户模式下部分系统目录不可读，报告会标注归因缺口",
	}
}
