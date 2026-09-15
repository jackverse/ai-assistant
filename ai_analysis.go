package main

// 扫描结果的 AI 分析：把扫描事实（路径、占用、大小、最近使用时间）
// 压缩成紧凑的 JSON 摘要交给大模型，流式回传分析结论。
//
// 设计要点（用户要求）：AI 是全局能力——只要配置了大模型，
// 扫描页就能用 AI 分析，不新增页面元素，只有一个按钮 + 一个结果面板。
// 隐私边界：发送的是路径与统计信息，且只发往用户自己在设置里填写的接口。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"winclean/internal/ai"
	"winclean/internal/model"
	"winclean/internal/sys"
	"winclean/internal/winapi"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const scanAISysPrompt = `你是 Windows 磁盘空间分析助手。用户会给你一份磁盘扫描事实（JSON：目录占用、大文件、最近修改时间、占用状态）。请输出：

【建议删除】每项一行：路径 ｜ 大小 ｜ 理由（结合最近修改时间与路径语义判断，如"半年未动的安装包残留"）｜ 预计可释放
【需确认】可能有用但不能确定的，说明不确定点
【不要删除】原因（系统组件/用户数据/凭据/正在使用），这一组必须保守
【正在使用】被进程占用的文件，标注"运行中依赖，暂勿动"
【建议迁移】大且可搬迁的（包管理器缓存、运行时、仓库），给出可搬的方向
最后给一行【总建议】。

硬性规则：
- 只基于给定事实判断，禁止编造扫描数据里不存在的路径或用途
- 路径含 Windows、Program Files、System32 的一律【不要删除】
- 最近修改时间在 30 天内的用户数据默认放【需确认】，不要建议删除
- 不确定就明说不确定
- 中文，紧凑，每项一行，不要客套话`

// scanAIBusy 防止重入。
var scanAIBusy atomic.Bool

// ScanAIState 是 AI 分析的进度快照。
type ScanAIState struct {
	Running bool   `json:"running"`
	Error   string `json:"error"`
}

func (a *App) scanAISettings() ai.Settings {
	c := a.cfg.AI
	return ai.Settings{BaseURL: c.BaseURL, APIKey: c.APIKey, Model: c.Model}
}

// AIConfigured 报告大模型是否已配置（决定扫描页要不要显示 AI 按钮）。
func (a *App) AIConfigured() bool { return a.scanAISettings().Valid() }

// ScanAIBusy 报告是否正在生成分析。
func (a *App) ScanAIBusy() bool { return scanAIBusy.Load() }

// StartScanAI 对最近一次扫描结果做 AI 分析（流式）。
//
// 事件：scanai:delta {delta}、scanai:done {content, error}。
func (a *App) StartScanAI() error {
	if !scanAIBusy.CompareAndSwap(false, true) {
		return errors.New("上一次 AI 分析还在生成中")
	}
	s := a.scanAISettings()
	if !s.Valid() {
		scanAIBusy.Store(false)
		return errors.New("AI 尚未配置：请先在「设置」填写接口信息")
	}

	a.mu.Lock()
	res := a.result
	a.mu.Unlock()
	if res == nil {
		scanAIBusy.Store(false)
		return errors.New("还没有扫描结果，请先执行一次扫描")
	}

	// 占用探测：只探最大的 20 个文件，几十次 syscall，代价可忽略。
	// 这是给 AI 的"是否在用"硬依据——没有它模型只能靠文件名猜。
	inUse := probeTopFiles(res.LargestFiles, 20)

	digest := buildScanDigest(res, inUse)

	ctx, cancel := context.WithCancel(context.Background())
	a.chatMu.Lock()
	a.chatCancel = cancel
	a.chatMu.Unlock()

	go func() {
		defer scanAIBusy.Store(false)
		defer cancel()

		messages := []ai.Message{
			{Role: "system", Content: scanAISysPrompt},
			{Role: "user", Content: "扫描事实：\n" + digest + "\n\n请按规则输出分析。"},
		}

		streamDelta := func(delta string) {
			if a.ctx != nil {
				runtime.EventsEmit(a.ctx, "scanai:delta", map[string]string{"delta": delta})
			}
		}

		content, err := ai.Stream(ctx, s, messages, streamDelta)
		errMsg := ""
		if err != nil {
			if ctx.Err() != nil {
				errMsg = "（已停止）"
			} else {
				errMsg = err.Error()
			}
		}
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "scanai:done", map[string]string{
				"content": content, "error": errMsg,
			})
		}
	}()

	return nil
}

// StopScanAI 停止正在生成的分析。
func (a *App) StopScanAI() {
	a.chatMu.Lock()
	cancel := a.chatCancel
	a.chatMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// probeTopFiles 对最大的若干文件做占用探测，返回被占用的路径集合。
func probeTopFiles(files []model.LargestFile, limit int) map[string]bool {
	out := map[string]bool{}
	n := 0
	for _, f := range files {
		if n >= limit {
			break
		}
		if f.IsReparse || f.OnDisk <= 0 {
			continue
		}
		if inUse, unknown := winapi.ProbeFileInUse(sys.ToExtendedPath(f.Path)); inUse && !unknown {
			out[f.Path] = true
			n++
		}
	}
	return out
}

// buildScanDigest 把扫描结果压缩成给大模型看的紧凑 JSON。
//
// 控制预算：目录取占用最大的 60 条、文件取最大的 20 个；
// 时间只保留日期、大小只保留 MB 一位小数——路径本身占 token 大头，
// 其余字段尽量精简。
func buildScanDigest(res *model.ScanResult, inUse map[string]bool) string {
	var b strings.Builder
	b.WriteString("{\n")

	// 卷概览
	b.WriteString(`"volume":[`)
	for i, v := range res.Volumes {
		if !v.Scanned && !v.IsSystem {
			continue
		}
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"letter":%q,"free_gb":%.1f,"total_gb":%.1f}`,
			v.Letter, float64(v.FreeBytes)/(1<<30), float64(v.TotalBytes)/(1<<30))
	}
	b.WriteString("],\n")

	// 目录：按占用取前 60 条
	dirs := append([]model.DirEntry(nil), res.Dirs...)
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].OnDisk > dirs[j].OnDisk })
	if len(dirs) > 60 {
		dirs = dirs[:60]
	}
	b.WriteString(`"top_dirs":[`)
	for i, d := range dirs {
		if d.OnDisk <= 0 || d.IsReparse {
			continue // 链接不占空间，对分析无信息量
		}
		if i > 0 {
			b.WriteString(",")
		}
		flags := []string{}
		if d.SummaryOnly {
			flags = append(flags, "仅汇总")
		}
		if d.Skipped {
			flags = append(flags, "部分未读")
		}
		mtime := ""
		if d.NewestMtime != nil {
			mtime = d.NewestMtime.Format("2006-01-02")
		}
		fmt.Fprintf(&b, `{"path":%q,"gb":%.2f,"files":%d,"newest":%q`,
			d.Path, float64(d.OnDisk)/(1<<30), d.Files, mtime)
		if len(flags) > 0 {
			fmt.Fprintf(&b, `,"flags":%q`, strings.Join(flags, "/"))
		}
		b.WriteString("}")
	}
	b.WriteString("],\n")

	// 大文件：前 20 个，带占用探测结果
	b.WriteString(`"largest_files":[`)
	for i, f := range res.LargestFiles {
		if i > 0 {
			b.WriteString(",")
		}
		mtime := ""
		if f.Mtime != nil {
			mtime = f.Mtime.Format("2006-01-02")
		}
		fmt.Fprintf(&b, `{"path":%q,"gb":%.2f,"mtime":%q,"in_use":%t`,
			f.Path, float64(f.OnDisk)/(1<<30), mtime, inUse[f.Path])
		if strings.HasSuffix(strings.ToLower(f.Path), ".vhdx") {
			b.WriteString(`,"kind":"虚拟磁盘"`)
		}
		b.WriteString("}")
	}
	b.WriteString("],\n")

	// 扫描警告（权限缺口等）
	fmt.Fprintf(&b, `"warnings":%q`, strings.Join(res.Warnings, "；"))
	b.WriteString("\n}")

	return b.String()
}

// 保证 time 引用（mtime 格式化在上方）。
var _ = time.Now
