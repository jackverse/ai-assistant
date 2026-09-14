package main

// 垃圾清理模块的前端绑定。
//
// 与扫描页同一套异步模式：Start 类方法立即返回，进度用轮询，
// 完成时广播事件。后端同一时刻只允许一个清理操作（扫描或执行互斥）。

import (
	"context"
	"errors"
	"fmt"

	"winclean/internal/clean"
	"winclean/internal/sys"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// 清理阈值护栏（docs/design/09 §4.1）。
const (
	cleanGuardSingleBytes  = 5 * 1024 * 1024 * 1024  // 单项可回收 > 5 GB 需确认
	cleanGuardSingleFiles  = 10000                    // 单项文件数 > 1 万需确认
	cleanGuardTotalBytes   = 20 * 1024 * 1024 * 1024 // 总量 > 20 GB 需确认
)

// CleanItemDTO 是清理候选 + 测量结果的合并视图。
type CleanItemDTO struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Level string `json:"level"` // L1 / L2
	Kind  string `json:"kind"`

	Files int64 `json:"files"`
	Size  int64 `json:"size"`
	// Reclaimable 是「现在就能删」的部分（已扣除近期文件与被占用文件）
	Reclaimable      int64 `json:"reclaimable"`
	ReclaimableFiles int64 `json:"reclaimable_files"`
	RecentFiles      int64 `json:"recent_files"`
	RecentBytes      int64 `json:"recent_bytes"`
	InUseFiles       int64 `json:"in_use_files"`
	InUseBytes       int64 `json:"in_use_bytes"`

	MinAgeHours   float64  `json:"min_age_hours"`
	AdminRequired bool     `json:"admin_required"`
	Blocked       string   `json:"blocked"`
	Reason        string   `json:"reason"`
	Consequence   string   `json:"consequence"`
	Note          string   `json:"note,omitempty"`
	DownloadFiles []FilePPDTO `json:"download_files,omitempty"`
}

// FilePPDTO 是下载目录里单条文件的展示项。
type FilePPDTO struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	IsDir     bool   `json:"is_dir"`
	Size      int64  `json:"size"`
	AgeDays   int64  `json:"age_days"`
	Suggested bool   `json:"suggested"`
}

// CleanProgressDTO 是候选检测阶段的轮询快照。
type CleanProgressDTO struct {
	Running bool   `json:"running"`
	Done    bool   `json:"done"`
	Error   string `json:"error"`
	Current string `json:"current"`
	Count   int    `json:"count"`
	Total   int    `json:"total"`
}

// CleanExecProgressDTO 是执行阶段的轮询快照。
type CleanExecProgressDTO struct {
	Running      bool   `json:"running"`
	Done         bool   `json:"done"`
	Error        string `json:"error"`
	CurrentName  string `json:"current_name"`
	FreedBytes   int64  `json:"freed_bytes"`
	DeletedFiles int64  `json:"deleted_files"`
}

// ───────── 检测（只读测量） ─────────

// CleanScan 启动一次候选检测，立即返回；进度用 CleanProgress() 轮询。
func (a *App) CleanScan() error {
	a.cleanMu.Lock()
	if a.cleanBusy {
		a.cleanMu.Unlock()
		return errors.New("已有清理操作在进行中")
	}
	a.cleanBusy = true
	a.cleanScanDone.Store(false)
	a.cleanExecDone.Store(false)
	a.cleanScanErr.Store("")
	a.cleanItems = nil
	a.cleanStats = nil
	a.cleanMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	a.cleanCancel = cancel

	go func() {
		defer func() {
			a.cleanMu.Lock()
			a.cleanBusy = false
			a.cleanMu.Unlock()
			cancel()
			if a.ctx != nil {
				runtime.EventsEmit(a.ctx, "clean:scan:finished")
			}
		}()

		cands := clean.Catalog()
		clean.SortCandidates(cands)
		stats := make(map[string]clean.Stats, len(cands))

		for i, c := range cands {
			if ctx.Err() != nil {
				a.cleanScanErr.Store("已取消")
				return
			}
			a.cleanScanCur.Store(c.Title)
			a.cleanScanCount.Store(int64(i))
			st, err := clean.Measure(ctx, c)
			if err != nil && ctx.Err() == nil {
				// 单个候选测量失败不拖垮整次检测，标记为不可用
				st.Exists = false
			}
			stats[c.ID] = st
		}
		a.cleanScanCount.Store(int64(len(cands)))

		a.cleanMu.Lock()
		a.cleanItems = cands
		a.cleanStats = stats
		a.cleanMu.Unlock()
		a.cleanScanDone.Store(true)
	}()

	return nil
}

// CleanProgress 返回候选检测进度。
func (a *App) CleanProgress() CleanProgressDTO {
	dto := CleanProgressDTO{
		Running: a.cleanBusyLoad(),
		Done:    a.cleanScanDone.Load(),
		Count:   int(a.cleanScanCount.Load()),
	}
	if s, ok := a.cleanScanErr.Load().(string); ok {
		dto.Error = s
	}
	dto.Current, _ = a.cleanScanCur.Load().(string)
	return dto
}

// CleanItems 返回检测完成的候选列表；未完成时返回 nil。
func (a *App) CleanItems() []CleanItemDTO {
	a.cleanMu.Lock()
	cands, stats := a.cleanItems, a.cleanStats
	a.cleanMu.Unlock()
	if cands == nil {
		return nil
	}
	elevated := sys.IsElevated()
	out := make([]CleanItemDTO, 0, len(cands))
	for _, c := range cands {
		dto := CleanItemDTO{
			ID: c.ID, Title: c.Title, Level: string(c.Level), Kind: string(c.Kind),
			MinAgeHours: c.MinAge.Hours(), AdminRequired: c.AdminRequired,
			Blocked: clean.BlockedReason(c, elevated),
			Reason: c.Reason, Consequence: c.Consequence, Note: c.Note,
		}
		if st, ok := stats[c.ID]; ok {
			dto.Files = st.Files
			dto.Size = st.Size
			dto.Reclaimable = st.Reclaimable
			dto.ReclaimableFiles = st.ReclaimableFiles
			dto.RecentFiles = st.RecentFiles
			dto.RecentBytes = st.RecentBytes
			dto.InUseFiles = st.InUseFiles
			dto.InUseBytes = st.InUseBytes
			for _, f := range st.DownloadFiles {
				dto.DownloadFiles = append(dto.DownloadFiles, FilePPDTO{
					Path: f.Path, Name: f.Name, IsDir: f.IsDir,
					Size: f.Size, AgeDays: f.AgeDays, Suggested: f.Suggested,
				})
			}
		}
		out = append(out, dto)
	}
	return out
}

// ───────── 执行 ─────────

// CleanExecute 执行勾选的候选，立即返回。
//
// downloadFiles 是下载目录（KindDownloads）里用户勾选的精确路径；
// 其它类型的删除范围完全由内置候选决定，前端无法注入任意路径。
func (a *App) CleanExecute(ids []string, downloadFiles []string, confirmLarge bool) error {
	a.cleanMu.Lock()
	if a.cleanBusy {
		a.cleanMu.Unlock()
		return errors.New("已有清理操作在进行中")
	}
	cands := a.cleanItems
	if cands == nil {
		a.cleanMu.Unlock()
		return errors.New("请先检测可清理项")
	}

	// 把 downloadFiles 按归属分发给对应候选
	byID := map[string]clean.Candidate{}
	for _, c := range cands {
		byID[c.ID] = c
	}
	filesByCand := map[string][]string{}
	for _, f := range downloadFiles {
		abs, err := sys.CleanAbsolute(f)
		if err != nil {
			continue
		}
		for _, c := range cands {
			if c.Kind != clean.KindDownloads {
				continue
			}
			for _, root := range c.Paths {
				if sys.IsUnder(abs, root) {
					filesByCand[c.ID] = append(filesByCand[c.ID], abs)
					break
				}
			}
		}
	}

	var sel []clean.Selection
	var totalReclaim, totalFiles int64
	var large []string
	for _, id := range ids {
		c, ok := byID[id]
		if !ok {
			continue
		}
		s := clean.Selection{ID: id, Files: filesByCand[id]}
		if c.Kind == clean.KindDownloads && len(s.Files) == 0 {
			continue // 下载类没勾任何文件就没有可执行的
		}
		sel = append(sel, s)

		if st, ok := a.cleanStats[id]; ok {
			totalReclaim += st.Reclaimable
			totalFiles += st.ReclaimableFiles
			if st.Reclaimable > cleanGuardSingleBytes || st.ReclaimableFiles > cleanGuardSingleFiles {
				large = append(large, fmt.Sprintf("%s（可回收 %d 个文件 / %s）",
					c.Title, st.ReclaimableFiles, humanBytesLocal(st.Reclaimable)))
			}
		}
	}
	if len(sel) == 0 {
		a.cleanMu.Unlock()
		return errors.New("没有勾选任何可清理项")
	}
	if !confirmLarge && (totalReclaim > cleanGuardTotalBytes || len(large) > 0) {
		msg := ""
		if len(large) > 0 {
			msg = "以下候选体量较大：" + joinStrings(large, "、")
		} else {
			msg = fmt.Sprintf("合计可回收 %s", humanBytesLocal(totalReclaim))
		}
		a.cleanMu.Unlock()
		return fmt.Errorf("NEED_CONFIRM:%s。请确认后再执行", msg)
	}

	a.cleanBusy = true
	a.cleanExecDone.Store(false)
	a.cleanExecErr.Store("")
	a.cleanResults = nil
	a.cleanMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	a.cleanCancel = cancel

	go func() {
		defer func() {
			a.cleanMu.Lock()
			a.cleanBusy = false
			a.cleanMu.Unlock()
			cancel()
			if a.ctx != nil {
				runtime.EventsEmit(a.ctx, "clean:exec:finished")
			}
		}()

		results := clean.Execute(ctx, cands, sel, func(p clean.Progress) {
			a.cleanExecName.Store(p.CandidateName)
			a.cleanExecFreed.Store(p.FreedBytes)
			a.cleanExecFiles.Store(p.DeletedFiles)
		})
		a.cleanMu.Lock()
		a.cleanResults = results
		a.cleanMu.Unlock()
		a.cleanExecDone.Store(true)
	}()

	return nil
}

// CleanExecProgress 返回执行进度。
func (a *App) CleanExecProgress() CleanExecProgressDTO {
	dto := CleanExecProgressDTO{
		Running:      a.cleanBusyLoad(),
		Done:         a.cleanExecDone.Load(),
		FreedBytes:   a.cleanExecFreed.Load(),
		DeletedFiles: a.cleanExecFiles.Load(),
	}
	dto.CurrentName, _ = a.cleanExecName.Load().(string)
	if s, ok := a.cleanExecErr.Load().(string); ok {
		dto.Error = s
	}
	return dto
}

// CleanResults 返回执行结果（执行结束后）。
//
// 用 main 包 DTO 转换而非直接返回 internal/clean 类型：
// app.go 记录过 Wails 绑定对某些 internal 类型会静默丢弃方法，
// DTO 一层转换是实测可靠的形态。
type CleanResultDTO struct {
	ID             string   `json:"id"`
	OK             bool     `json:"ok"`
	FreedBytes     int64    `json:"freed_bytes"`
	DeletedFiles   int64    `json:"deleted_files"`
	SkippedInUse   int64    `json:"skipped_in_use"`
	SkippedRecent  int64    `json:"skipped_recent"`
	SkippedReparse int64    `json:"skipped_reparse"`
	Errors         int64    `json:"errors"`
	Samples        []string `json:"samples,omitempty"`
}

// CleanResults 返回执行结果（执行结束后）。
func (a *App) CleanResults() []CleanResultDTO {
	a.cleanMu.Lock()
	results := a.cleanResults
	a.cleanMu.Unlock()
	if results == nil {
		return nil
	}
	out := make([]CleanResultDTO, 0, len(results))
	for _, r := range results {
		out = append(out, CleanResultDTO{
			ID: r.ID, OK: r.OK,
			FreedBytes: r.FreedBytes, DeletedFiles: r.DeletedFiles,
			SkippedInUse: r.SkippedInUse, SkippedRecent: r.SkippedRecent,
			SkippedReparse: r.SkippedReparse, Errors: r.Errors, Samples: r.Samples,
		})
	}
	return out
}

// CancelClean 请求取消当前清理操作（检测或执行）。
func (a *App) CancelClean() {
	a.cleanMu.Lock()
	cancel := a.cleanCancel
	a.cleanMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (a *App) cleanBusyLoad() bool {
	a.cleanMu.Lock()
	defer a.cleanMu.Unlock()
	return a.cleanBusy
}

func joinStrings(items []string, sep string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}

func humanBytesLocal(n int64) string { return fmtBytesLocal(n) }

// fmtBytesLocal 与前端格式化保持一致的轻量实现。
func fmtBytesLocal(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	v := float64(n)
	i := -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}
