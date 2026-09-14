package main

// 空间迁移模块的前端绑定。
//
// 模式与 clean_api.go 相同：异步启动 + 轮询进度 + 完成事件。
// 执行逻辑（预检/复制/校验/切换/记账/撤销）全部在 internal/migrate，
// 这一层只做 DTO 转换与并发互斥。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"winclean/internal/migrate"
	"winclean/internal/safeio"
	"winclean/internal/sys"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// MigrateProgressDTO 是候选检测阶段的轮询快照。
type MigrateProgressDTO struct {
	Running bool   `json:"running"`
	Done    bool   `json:"done"`
	Error   string `json:"error"`
	Current string `json:"current"`
	Count   int    `json:"count"`
	Total   int    `json:"total"`
}

// MigrateExecProgressDTO 是执行阶段的轮询快照。
type MigrateExecProgressDTO struct {
	Running     bool   `json:"running"`
	Done        bool   `json:"done"`
	Error       string `json:"error"`
	Phase       string `json:"phase"`
	ItemName    string `json:"item_name"`
	CurrentFile string `json:"current_file"`
	CopiedFiles int64  `json:"copied_files"`
	CopiedBytes int64  `json:"copied_bytes"`
}

// ───────── 候选检测 ─────────

// MigrateScan 启动候选检测（发现 + 测量大小），立即返回。
func (a *App) MigrateScan() error {
	a.migMu.Lock()
	if a.migBusy {
		a.migMu.Unlock()
		return errors.New("已有迁移操作在进行中")
	}
	a.migBusy = true
	a.migScanDone.Store(false)
	a.migExecDone.Store(false)
	a.migScanErr.Store("")
	a.migCands = nil
	a.migMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	a.migCancel = cancel

	go func() {
		defer func() {
			a.migMu.Lock()
			a.migBusy = false
			a.migMu.Unlock()
			cancel()
			if a.ctx != nil {
				runtime.EventsEmit(a.ctx, "migrate:scan:finished")
			}
		}()

		// 保留用户手动添加的自定义目录（它们不随内置清单重扫而消失）
		a.migMu.Lock()
		var customs []migrate.Cand
		for _, c := range a.migCands {
			if strings.HasPrefix(c.ID, "custom:") {
				customs = append(customs, c)
			}
		}
		a.migCands = nil
		a.migMu.Unlock()

		cands := migrate.Catalog()
		for i, c := range cands {
			if ctx.Err() != nil {
				a.migScanErr.Store("已取消")
				return
			}
			a.migScanCur.Store(c.Name)
			a.migScanCount.Store(int64(i))
			if sum, err := safeio.SumTree(ctx, c.Path); err == nil {
				c.Size = sum.Bytes
			}
			cands[i] = c
		}
		cands = append(cands, customs...)
		a.migScanCount.Store(int64(len(cands)))

		a.migMu.Lock()
		a.migCands = cands
		a.migMu.Unlock()
		a.migScanDone.Store(true)
	}()

	return nil
}

// MigrateProgress 返回候选检测进度。
func (a *App) MigrateProgress() MigrateProgressDTO {
	dto := MigrateProgressDTO{
		Running: a.migBusyLoad(),
		Done:    a.migScanDone.Load(),
		Count:   int(a.migScanCount.Load()),
	}
	if s, ok := a.migScanErr.Load().(string); ok {
		dto.Error = s
	}
	dto.Current, _ = a.migScanCur.Load().(string)
	return dto
}

// MigrateItemDTO 是迁移候选的展示视图（main 包 DTO，原因见 CleanResults）。
type MigrateItemDTO struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	App     string `json:"app,omitempty"`
	AppIcon string `json:"app_icon,omitempty"`
	Role    string `json:"role,omitempty"`
	EnvVar  string `json:"env_var,omitempty"`
	EnvNow  string `json:"env_now,omitempty"`
	Risk    string `json:"risk"`
	RiskWhy string `json:"risk_why,omitempty"`
	Note    string `json:"note,omitempty"`
	// Custom 标记这是用户手动添加的目录（界面上可被移除）。
	Custom bool `json:"custom,omitempty"`
}

func candToDTO(c migrate.Cand, custom bool) MigrateItemDTO {
	return MigrateItemDTO{
		ID: c.ID, Name: c.Name, Path: c.Path, Size: c.Size,
		App: c.App, AppIcon: c.AppIcon, Role: string(c.Role),
		EnvVar: c.EnvVar, EnvNow: c.EnvNow,
		Risk: string(c.Risk), RiskWhy: c.RiskWhy, Note: c.Note,
		Custom: custom,
	}
}

// MigrateItems 返回检测完成的候选列表；未完成时返回 nil。
func (a *App) MigrateItems() []MigrateItemDTO {
	a.migMu.Lock()
	cands := a.migCands
	a.migMu.Unlock()
	if cands == nil {
		return nil
	}
	out := make([]MigrateItemDTO, 0, len(cands))
	for _, c := range cands {
		out = append(out, candToDTO(c, strings.HasPrefix(c.ID, "custom:")))
	}
	return out
}

// MigrateAddPath 把用户指定的任意目录添加为迁移项（工作/数据/缓存地址
// 不在已知清单里时的通用入口），同步测量大小后返回。
//
// role 取 "work"/"data"/"cache"（默认 data）；label 为空时用目录名。
// 系统目录（C:\Windows、Program Files 等）会被拒绝——执行层预检也会再拦一次。
func (a *App) MigrateAddPath(path, role, label string) (MigrateItemDTO, error) {
	abs, err := sys.CleanAbsolute(path)
	if err != nil {
		return MigrateItemDTO{}, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return MigrateItemDTO{}, fmt.Errorf("目录不可访问: %w", err)
	}
	if !fi.IsDir() {
		return MigrateItemDTO{}, errors.New("请选择目录而不是文件")
	}
	if is, _, err := sys.IsReparsePoint(abs); err == nil && is {
		return MigrateItemDTO{}, errors.New("该路径已是目录联接（可能迁移过）")
	}
	for _, root := range []string{"C:\\Windows", "C:\\Program Files", "C:\\Program Files (x86)"} {
		if sys.IsUnder(abs, root) {
			return MigrateItemDTO{}, fmt.Errorf("拒绝添加系统/程序目录 %s", abs)
		}
	}

	switch role {
	case "work":
		role = string(migrate.RoleWork)
	case "cache":
		role = string(migrate.RoleCache)
	default:
		role = string(migrate.RoleData)
	}
	if strings.TrimSpace(label) == "" {
		label = filepath.Base(abs)
	}

	a.migMu.Lock()
	defer a.migMu.Unlock()
	// 已存在（同路径）则不重复添加
	for _, c := range a.migCands {
		if strings.EqualFold(c.Path, abs) {
			return candToDTO(c, strings.HasPrefix(c.ID, "custom:")), nil
		}
	}
	a.migCustomSeq++
	c := migrate.Cand{
		ID:   fmt.Sprintf("custom:%d", a.migCustomSeq),
		Name: strings.TrimSpace(label), Path: abs,
		App: "自定义目录", AppIcon: "📁",
		Role: migrate.Role(role), Risk: migrate.RiskMedium,
		RiskWhy: "自定义目录：请自行确认对应软件已退出、该目录确可搬迁",
	}
	if sum, err := safeio.SumTree(context.Background(), abs); err == nil {
		c.Size = sum.Bytes
	}
	a.migCands = append(a.migCands, c)
	return candToDTO(c, true), nil
}

// MigrateResultDTO 是单个候选的迁移结果视图。
type MigrateResultDTO struct {
	ID        string `json:"id"`
	OK        bool   `json:"ok"`
	Skipped   bool   `json:"skipped,omitempty"`
	Error     string `json:"error,omitempty"`
	Source    string `json:"source,omitempty"`
	Target    string `json:"target,omitempty"`
	Backup    string `json:"backup,omitempty"`
	Bytes     int64  `json:"bytes,omitempty"`
	Files     int64  `json:"files,omitempty"`
	EnvSet    bool   `json:"env_set,omitempty"`
	EnvOld    string `json:"env_old,omitempty"`
	EnvOldSet bool   `json:"env_old_set,omitempty"`
}

// MigrateResults 返回执行结果。
func (a *App) MigrateResults() []MigrateResultDTO {
	a.migMu.Lock()
	results := a.migResults
	a.migMu.Unlock()
	if results == nil {
		return nil
	}
	out := make([]MigrateResultDTO, 0, len(results))
	for _, r := range results {
		out = append(out, MigrateResultDTO{
			ID: r.ID, OK: r.OK, Skipped: r.Skipped, Error: r.Error,
			Source: r.Source, Target: r.Target, Backup: r.Backup,
			Bytes: r.Bytes, Files: r.Files, EnvSet: r.EnvSet,
			EnvOld: r.EnvOld, EnvOldSet: r.EnvOldSet,
		})
	}
	return out
}

// MigrateJournalItemDTO / MigrateJournalDTO 是迁移记录（撤销界面）的视图。
type MigrateJournalItemDTO struct {
	ID        string `json:"id"`
	Source    string `json:"source"`
	Target    string `json:"target"`
	Backup    string `json:"backup"`
	Bytes     int64  `json:"bytes"`
	Files     int64  `json:"files"`
	EnvVar    string `json:"env_var,omitempty"`
	EnvOld    string `json:"env_old,omitempty"`
	EnvOldSet bool   `json:"env_old_set,omitempty"`
	Done      bool   `json:"done"`
	Undone    bool   `json:"undone"`
	Purged    bool   `json:"purged"`
}

type MigrateJournalDTO struct {
	ID    string                   `json:"id"`
	Path  string                   `json:"path"`
	Time  string                   `json:"time"`
	Items []MigrateJournalItemDTO  `json:"items"`
	Err   string                   `json:"err,omitempty"`
}

// MigrateRemoveCustom 移除一个自定义迁移项（仅限 custom: 前缀）。
func (a *App) MigrateRemoveCustom(id string) error {
	if !strings.HasPrefix(id, "custom:") {
		return errors.New("内置迁移项不可移除")
	}
	a.migMu.Lock()
	defer a.migMu.Unlock()
	out := a.migCands[:0]
	for _, c := range a.migCands {
		if c.ID != id {
			out = append(out, c)
		}
	}
	a.migCands = out
	return nil
}

// MigrateJournals 返回全部迁移记录（撤销界面的数据源）。
func (a *App) MigrateJournals() []MigrateJournalDTO {
	recs := migrate.ListJournals()
	if recs == nil {
		return nil
	}
	out := make([]MigrateJournalDTO, 0, len(recs))
	for _, r := range recs {
		dto := MigrateJournalDTO{ID: r.ID, Path: r.Path, Err: r.Err}
		if !r.Time.IsZero() {
			dto.Time = r.Time.Format(time.RFC3339)
		}
		for _, it := range r.Items {
			dto.Items = append(dto.Items, MigrateJournalItemDTO{
				ID: it.ID, Source: it.Source, Target: it.Target, Backup: it.Backup,
				Bytes: it.Bytes, Files: it.Files,
				EnvVar: it.EnvVar, EnvOld: it.EnvOld, EnvOldSet: it.EnvOldSet,
				Done: it.Done, Undone: it.Undone, Purged: it.Purged,
			})
		}
		out = append(out, dto)
	}
	return out
}

// ───────── 执行 ─────────

// MigrateExecute 执行迁移，立即返回。
//
// targetBase 是目标根目录（如 D:\winclean-migrated）；
// setEnv 为 true 时对声明了环境变量的候选一并设置用户级变量。
func (a *App) MigrateExecute(ids []string, targetBase string, setEnv bool, confirmLarge bool) error {
	a.migMu.Lock()
	if a.migBusy {
		a.migMu.Unlock()
		return errors.New("已有迁移操作在进行中")
	}
	cands := a.migCands
	if cands == nil {
		a.migMu.Unlock()
		return errors.New("请先检测可迁移项")
	}
	if strings.TrimSpace(targetBase) == "" {
		a.migMu.Unlock()
		return errors.New("请先选择目标位置")
	}
	a.migBusy = true
	a.migExecDone.Store(false)
	a.migExecErr.Store("")
	a.migResults = nil
	a.migMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	a.migCancel = cancel

	go func() {
		defer func() {
			a.migMu.Lock()
			a.migBusy = false
			a.migMu.Unlock()
			cancel()
			if a.ctx != nil {
				runtime.EventsEmit(a.ctx, "migrate:exec:finished")
			}
		}()

		results := migrate.Run(ctx, cands, ids, migrate.Options{
			Base:         targetBase,
			SetEnvVars:   setEnv,
			ConfirmLarge: confirmLarge,
		}, func(p migrate.Progress) {
			a.migPhase.Store(p.Phase)
			a.migItemName.Store(p.ItemName)
			a.migCurFile.Store(p.CurrentFile)
			a.migCopiedBytes.Store(p.CopiedBytes)
			a.migCopiedFiles.Store(p.CopiedFiles)
		})

		// 把「需要用户确认」的哨兵翻译成显式错误，让前端弹确认框
		for _, r := range results {
			if r.ID == "__confirm__" && strings.HasPrefix(r.Error, "NEED_CONFIRM:") {
				if !confirmLarge {
					a.migExecErr.Store(strings.TrimPrefix(r.Error, "NEED_CONFIRM:"))
					a.migResults = nil
					a.migExecDone.Store(true)
					return
				}
				continue
			}
		}

		a.migMu.Lock()
		a.migResults = results
		a.migMu.Unlock()
		a.migExecDone.Store(true)
	}()

	return nil
}

// MigrateExecProgress 返回执行进度。
func (a *App) MigrateExecProgress() MigrateExecProgressDTO {
	dto := MigrateExecProgressDTO{
		Running: a.migBusyLoad(),
		Done:    a.migExecDone.Load(),
	}
	dto.Phase, _ = a.migPhase.Load().(string)
	dto.ItemName, _ = a.migItemName.Load().(string)
	dto.CurrentFile, _ = a.migCurFile.Load().(string)
	dto.CopiedBytes = a.migCopiedBytes.Load()
	dto.CopiedFiles = a.migCopiedFiles.Load()
	if s, ok := a.migExecErr.Load().(string); ok {
		dto.Error = s
	}
	return dto
}

// ───────── journal：撤销与延迟清理 ─────────

// MigrateUndo 整体撤销一次迁移：摘链接、备份改回原名、恢复环境变量。
func (a *App) MigrateUndo(journalID string) error {
	if err := migrate.Undo(journalID); err != nil {
		return err
	}
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "migrate:journal:changed")
	}
	return nil
}

// MigratePurgeBackup 延迟清理：删除迁移备份，真正释放源盘空间。
//
// 只在「原路径确为我们创建的 Junction 且指向正确」时才动手；
// 清理后无法再回到迁移前状态（数据只在新位置）。
func (a *App) MigratePurgeBackup(journalID string) error {
	if err := migrate.PurgeBackup(journalID); err != nil {
		return err
	}
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "migrate:journal:changed")
	}
	return nil
}

// CancelMigrate 请求取消当前迁移操作。
func (a *App) CancelMigrate() {
	a.migMu.Lock()
	cancel := a.migCancel
	a.migMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (a *App) migBusyLoad() bool {
	a.migMu.Lock()
	defer a.migMu.Unlock()
	return a.migBusy
}
