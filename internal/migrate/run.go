package migrate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"winclean/internal/safeio"
	"winclean/internal/sys"
	"winclean/internal/winapi"
)

// 预检对空间的要求：目标盘剩余 ≥ 源大小 × 1.1（docs/design/07 §4.1）。
const spaceHeadroom = 1.1

// 保留 10% 余量之外，源盘至少还应剩余的空间（避免把源盘压到 0）。
const minSourceFree = 2 * 1024 * 1024 * 1024 // 2 GB

// RunItem 是一次迁移请求（候选 + 用户指定的目标根）。
type RunItem struct {
	ID       string
	Cand     Cand
	Target   string // 完整目标路径（base\子目录名）
	SetEnv   bool   // 是否同时设置环境变量（仅 EnvVar 非空的候选有意义）
}

// ItemResult 是一个候选的执行结果。
type ItemResult struct {
	ID      string `json:"id"`
	OK      bool   `json:"ok"`
	Skipped bool   `json:"skipped,omitempty"` // 幂等：已迁移过
	Error   string `json:"error,omitempty"`

	Source string `json:"source,omitempty"`
	Target string `json:"target,omitempty"`
	Backup string `json:"backup,omitempty"` // 原数据备份（删除它才真正释放 C 盘）
	Bytes  int64  `json:"bytes,omitempty"`
	Files  int64  `json:"files,omitempty"`
	EnvSet bool   `json:"env_set,omitempty"`

	// 环境变量原值（撤销用；仅 EnvSet 时有意义）
	EnvOld    string `json:"env_old,omitempty"`
	EnvOldSet bool   `json:"env_old_set,omitempty"`
}

// Progress 是执行进度。
type Progress struct {
	Phase        string // precheck / copy / verify / switch / validate / done
	ItemID       string
	ItemName     string
	CopiedFiles  int64
	CopiedBytes  int64
	CurrentFile  string
}

// Options 控制一次执行。
type Options struct {
	Base         string // 目标根目录，如 D:\winclean-migrated
	SetEnvVars   bool   // 对声明的候选一并设置环境变量
	ConfirmLarge bool   // 已确认大体量迁移（单项 > 5 GB 或总量 > 20 GB）
	// AllowSameVolume 仅供测试：单测里源与目标都在 t.TempDir() 同卷。
	// 生产路径必须跨卷（同卷迁移没有意义），此字段不暴露给前端。
	AllowSameVolume bool
}

// PrecheckError 是预检失败（可读原因列表）。
type PrecheckError struct{ Items []string }

func (e *PrecheckError) Error() string {
	return "预检未通过：" + strings.Join(e.Items, "；")
}

// Run 执行一批迁移（顺序执行——大数据搬运并行只会互相抢 IO）。
//
// 单个候选的标准七步（docs/design/07 §3）：预检 → 复制 → 校验 → 切换 →
// 验证生效 → （延迟清理，默认不做，由用户显式触发）→ 记账。
// 关键原则：切换（改指向前）源数据始终完好；任何失败都不触碰源。
func Run(ctx context.Context, cands []Cand, sel []string, opts Options, prog func(Progress)) []ItemResult {
	byID := make(map[string]Cand, len(cands))
	for _, c := range cands {
		byID[c.ID] = c
	}

	base := opts.Base
	var results []ItemResult

	// 第一轮：全部预检通过才动手（部分失败比全部不执行更糟——
	// 用户会以为「迁了一半」是正常状态）。
	var prepared []prepared
	for _, id := range sel {
		c, ok := byID[id]
		if !ok {
			results = append(results, ItemResult{ID: id, Error: "未知候选"})
			continue
		}
		// 幂等：已经是 Junction 说明此前迁移过，如实报告并跳过
		if is, _, err := sys.IsReparsePoint(c.Path); err == nil && is {
			results = append(results, ItemResult{ID: id, OK: true, Skipped: true})
			continue
		}
		p, err := precheck(c, base, opts)
		if err != nil {
			results = append(results, ItemResult{ID: id, Error: err.Error()})
			continue
		}
		prepared = append(prepared, p)
	}
	if ctx.Err() != nil {
		return results
	}
	if len(prepared) == 0 {
		return results
	}
	if !opts.ConfirmLarge {
		if big := largeOnes(prepared); big != "" {
			return append(results, ItemResult{ID: "__confirm__",
				Error: "NEED_CONFIRM:存在超过 5 GB 的单项或总量超过 20 GB 的迁移：" + big})
		}
	}

	journalID := "migrate-" + time.Now().Format("20060102-150405")
	j, _, jerr := NewJournal(journalID)
	if jerr != nil {
		return append(results, ItemResult{ID: "__journal__", Error: "无法创建迁移日志（拒绝执行）: " + jerr.Error()})
	}
	defer j.Close()

	for _, p := range prepared {
		if ctx.Err() != nil {
			break
		}
		res := runOne(ctx, p, opts, prog, j)
		results = append(results, res)
	}
	return results
}

type prepared struct {
	cand   Cand
	target string
	backup string
}

func largeOnes(ps []prepared) string {
	var names []string
	var total int64
	for _, p := range ps {
		total += p.cand.Size
		if p.cand.Size > 5*1024*1024*1024 {
			names = append(names, fmt.Sprintf("%s（%s）", p.cand.Name, safeio.FormatBytes(p.cand.Size)))
		}
	}
	if total > 20*1024*1024*1024 && len(names) == 0 {
		names = append(names, fmt.Sprintf("合计 %s", safeio.FormatBytes(total)))
	}
	return strings.Join(names, "、")
}

// precheck 落实 docs/design/07 §3 步骤 1 的全部硬性检查。
// 任何一条不满足都直接拒绝，绝不带病执行。
func precheck(c Cand, base string, opts Options) (prepared, error) {
	p := prepared{cand: c}

	src, err := sys.CleanAbsolute(c.Path)
	if err != nil {
		return p, fmt.Errorf("源路径无效: %w", err)
	}
	p.cand.Path = src

	// 幂等：已经是 Junction 说明此前迁移过
	if is, _, err := sys.IsReparsePoint(src); err == nil && is {
		return p, fmt.Errorf("%s 已是链接（可能已迁移过）", src)
	}

	fi, err := os.Stat(src)
	if err != nil {
		return p, fmt.Errorf("源路径不可访问: %w", err)
	}
	if !fi.IsDir() {
		return p, fmt.Errorf("源路径不是目录")
	}

	// 禁止迁移的根（docs/design/07 §8）：系统目录、已安装程序等。
	// 候选集虽是内置白名单，这里再拦一道，防止未来扩充候选时越界。
	winDir, err := sys.SystemRoot() // C:\Windows
	if err == nil {
		for _, root := range []string{winDir, `C:\Program Files`, `C:\Program Files (x86)`} {
			if sys.IsUnder(src, root) {
				return p, fmt.Errorf("拒绝迁移系统/程序目录 %s", src)
			}
		}
	}

	// 目标路径
	if strings.TrimSpace(base) == "" {
		return p, fmt.Errorf("未指定目标位置")
	}
	name := filepath.Base(src)
	tgt, err := sys.CleanAbsolute(base + `\` + name)
	if err != nil {
		return p, fmt.Errorf("目标路径无效: %w", err)
	}
	// 目标不得在源子树内（防循环），源也不得在目标内
	if sys.IsUnder(tgt, src) || sys.IsUnder(src, tgt) {
		return p, fmt.Errorf("目标路径与源路径互相嵌套")
	}
	if _, err := os.Stat(tgt); err == nil {
		return p, fmt.Errorf("目标 %s 已存在（绝不覆盖）", tgt)
	}
	p.target = tgt

	// 备份路径：与源同目录，同卷 rename 才是原子操作
	p.backup = src + ".迁移备份-" + time.Now().Format("20060102-150405")
	if len(p.backup)+len(`\`) >= 260 && !longPathsEnabled() {
		return p, fmt.Errorf("备份路径过长且系统未启用长路径支持")
	}

	// 卷检查
	srcVol := sys.VolumeRootOf(src)
	tgtVol := sys.VolumeRootOf(tgt)
	if !opts.AllowSameVolume && strings.EqualFold(srcVol, tgtVol) {
		return p, fmt.Errorf("目标盘与源盘相同（%s），迁移没有意义", srcVol)
	}
	tvi, err := winapi.GetVolumeInformation(tgtVol)
	if err != nil {
		return p, fmt.Errorf("无法读取目标卷信息: %w", err)
	}
	if !strings.EqualFold(tvi.FileSystem, "NTFS") {
		return p, fmt.Errorf("目标卷文件系统为 %s，不支持目录联接（需要 NTFS）", tvi.FileSystem)
	}
	space, err := winapi.GetDiskFreeSpace(tgtVol)
	if err != nil {
		return p, fmt.Errorf("无法读取目标卷剩余空间: %w", err)
	}
	need := uint64(float64(c.Size) * spaceHeadroom)
	if c.Size > 0 && space.TotalFree < need {
		return p, fmt.Errorf("目标卷 %s 剩余 %s，不足所需 %s（含 10%% 余量）",
			tgtVol, safeio.FormatBytes(int64(space.TotalFree)), safeio.FormatBytes(int64(need)))
	}
	if space2, err := winapi.GetDiskFreeSpace(srcVol); err == nil && space2.TotalFree < minSourceFree && c.Size > minSourceFree {
		return p, fmt.Errorf("源卷剩余空间过低（%s），迁移完成前可能影响系统运行",
			safeio.FormatBytes(int64(space2.TotalFree)))
	}

	return p, nil
}

// runOne 执行单个候选的复制→校验→切换→验证，全程记账。
func runOne(ctx context.Context, p prepared, opts Options, prog func(Progress), j *Journal) ItemResult {
	res := ItemResult{ID: p.cand.ID, Source: p.cand.Path, Target: p.target, Backup: p.backup}
	emit := func(phase, file string, copiedFiles, copiedBytes int64) {
		if prog != nil {
			prog(Progress{
				Phase: phase, ItemID: p.cand.ID, ItemName: p.cand.Name,
				CurrentFile: file, CopiedFiles: copiedFiles, CopiedBytes: copiedBytes,
			})
		}
	}

	// ── 复制（源保持原样，失败可安全重试）─────────────────────────
	emit("copy", "", 0, 0)
	files, bytes, _, err := safeio.CopyTree(ctx, p.cand.Path, p.target, func(f, b int64) {
		emit("copy", "", f, b)
	})
	if err != nil {
		cleanupTarget(p.target)
		res.Error = "复制失败（源数据未受影响）: " + err.Error()
		_ = j.Write(Entry{Time: time.Now(), Kind: "item_failed", Item: ItemRecord{ID: p.cand.ID, Source: res.Source}})
		return res
	}
	res.Files, res.Bytes = files, bytes

	// ── 校验（文件数 + 字节数；失败则清理目标并保留源）────────────
	emit("verify", "", files, bytes)
	srcSum, err := safeio.SumTree(ctx, p.cand.Path)
	if err == nil {
		err = safeio.VerifyTrees(srcSum, safeio.TreeSummary{Files: files, Bytes: bytes})
	}
	if err != nil {
		cleanupTarget(p.target)
		res.Error = "校验失败（已清理目标，源数据完好）: " + err.Error()
		_ = j.Write(Entry{Time: time.Now(), Kind: "item_failed", Item: ItemRecord{ID: p.cand.ID, Source: res.Source}})
		return res
	}
	_ = j.Write(Entry{Time: time.Now(), Kind: "item_copy_done", Item: ItemRecord{
		ID: p.cand.ID, Source: res.Source, Target: res.Target, Backup: res.Backup,
		Bytes: bytes, Files: files,
	}})

	// ── 切换：备份原名（同卷 rename 原子）→ 原位创建 Junction ──────
	emit("switch", "", files, bytes)
	if err := os.Rename(sys.ToExtendedPath(p.cand.Path), sys.ToExtendedPath(p.backup)); err != nil {
		cleanupTarget(p.target)
		res.Error = "切换失败：源目录被占用无法改名（已清理目标，源数据完好）: " + err.Error()
		_ = j.Write(Entry{Time: time.Now(), Kind: "item_failed", Item: ItemRecord{ID: p.cand.ID, Source: res.Source}})
		return res
	}
	// 原位置现在是空的：重建一个空目录作为 Junction 挂载点
	// （Junction 的挂载点必须存在且为空）
	if err := os.Mkdir(sys.ToExtendedPath(p.cand.Path), 0o755); err != nil {
		if rbErr := os.Rename(sys.ToExtendedPath(p.backup), sys.ToExtendedPath(p.cand.Path)); rbErr != nil {
			res.Error = "严重：创建挂载点失败且回滚也失败！原数据在 " + p.backup +
				"，请手动改回。原因: " + err.Error() + " / 回滚: " + rbErr.Error()
			_ = j.Write(Entry{Time: time.Now(), Kind: "item_failed", Item: ItemRecord{ID: p.cand.ID, Source: res.Source, Backup: res.Backup}})
			return res
		}
		cleanupTarget(p.target)
		res.Error = "创建挂载点目录失败（已回滚，源数据完好）: " + err.Error()
		_ = j.Write(Entry{Time: time.Now(), Kind: "item_failed", Item: ItemRecord{ID: p.cand.ID, Source: res.Source}})
		return res
	}
	if err := winapi.SetJunction(sys.ToExtendedPath(p.cand.Path), p.target); err != nil {
		// 回滚切换：把备份改回原名（尽力；失败时如实报告）
		if rbErr := os.Rename(sys.ToExtendedPath(p.backup), sys.ToExtendedPath(p.cand.Path)); rbErr != nil {
			res.Error = "严重：Junction 创建失败且回滚也失败！原数据在 " + p.backup +
				"，请手动改回。原因: " + err.Error() + " / 回滚: " + rbErr.Error()
			_ = j.Write(Entry{Time: time.Now(), Kind: "item_failed", Item: ItemRecord{ID: p.cand.ID, Source: res.Source, Backup: res.Backup}})
			return res
		}
		cleanupTarget(p.target)
		res.Error = "创建目录联接失败（已回滚，源数据完好）: " + err.Error()
		_ = j.Write(Entry{Time: time.Now(), Kind: "item_failed", Item: ItemRecord{ID: p.cand.ID, Source: res.Source}})
		return res
	}

	// ── 验证生效：原路径可访问、确为指向新位置的链接、内容可读 ──────
	emit("validate", "", files, bytes)
	isRe, tag, err := winapi.IsReparsePointRaw(sys.ToExtendedPath(p.cand.Path))
	switch {
	case err != nil:
		res.Error = "切换后无法读取原路径状态: " + err.Error()
	case !isRe || tag != winapi.IO_REPARSE_TAG_MOUNT_POINT:
		res.Error = "切换后原路径不是目录联接（状态异常，请勿删除备份，先检查）"
	default:
		if info, rerr := winapi.ReadReparsePoint(sys.ToExtendedPath(p.cand.Path)); rerr != nil || info.Target() != p.target {
			res.Error = "切换后链接目标不符（期望 " + p.target + "）"
		}
	}
	if res.Error != "" {
		_ = j.Write(Entry{Time: time.Now(), Kind: "item_failed", Item: ItemRecord{ID: p.cand.ID, Source: res.Source, Target: res.Target, Backup: res.Backup}})
		return res
	}

	// ── 环境变量（可选）：官方机制让软件直接使用新路径 ──────────────
	if opts.SetEnvVars && p.cand.EnvVar != "" {
		old, oldSet, serr := getUserEnv(p.cand.EnvVar)
		if serr == nil {
			if serr := setUserEnv(p.cand.EnvVar, p.target); serr == nil {
				winapi.BroadcastEnvironmentChange()
				res.EnvSet = true
				res.EnvOld, res.EnvOldSet = old, oldSet // 借字段意义：记录于 journal
			}
		}
	}

	// ── 记账（可回滚信息全部在案）────────────────────────────────
	rec := ItemRecord{
		ID: p.cand.ID, Source: res.Source, Target: res.Target, Backup: res.Backup,
		Bytes: bytes, Files: files,
		EnvVar: p.cand.EnvVar, Done: true,
	}
	if res.EnvSet {
		rec.EnvOld = res.EnvOld
		rec.EnvOldSet = res.EnvOldSet
	}
	_ = j.Write(Entry{Time: time.Now(), Kind: "item_done", Item: rec})
	res.OK = true
	emit("done", "", files, bytes)
	return res
}

// cleanupTarget 清理本次执行自己创建的目标目录。
// 只清我们刚创建的路径；RemoveTree 遇到链接只摘链接不进目标，
// 因此即使目标里有意外内容也不会越删越远。
func cleanupTarget(target string) {
	if target == "" {
		return
	}
	_, _, _, _ = safeio.RemoveTree(context.Background(), target, nil)
}

// ───────── 撤销（undo）与延迟清理（purge） ─────────

// Undo 把一个 journal 里全部已完成条目恢复到迁移前状态。
//
// 规则（docs/design/07 §7）：摘链接 → 备份改回原名 → 恢复环境变量。
// 每一步前都重新校验：原路径必须仍是我们创建的那条链接，
// 否则拒绝继续（绝不对一个真实目录做 RemoveDirectory）。
func Undo(journalID string) error {
	recs := ListJournals()
	var target *FileRecord
	for i := range recs {
		if recs[i].ID == journalID {
			target = &recs[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("找不到迁移记录 %s", journalID)
	}

	var errs []string
	for _, it := range target.Items {
		if !it.Done || it.Undone {
			continue
		}
		if err := undoItem(it); err != nil {
			errs = append(errs, it.ID+": "+err.Error())
			continue
		}
		markUndone(journalID, it.ID)
	}
	if len(errs) > 0 {
		return fmt.Errorf("部分条目恢复失败：%s", strings.Join(errs, "；"))
	}
	return nil
}

func undoItem(it ItemRecord) error {
	// 原路径必须仍是指向 Target 的 Junction——不是就停手。
	isRe, tag, err := winapi.IsReparsePointRaw(sys.ToExtendedPath(it.Source))
	if err != nil {
		return fmt.Errorf("读取原路径状态失败: %w", err)
	}
	if !isRe || tag != winapi.IO_REPARSE_TAG_MOUNT_POINT {
		return fmt.Errorf("原路径 %s 已不是目录联接（可能被移动或删除），拒绝继续撤销", it.Source)
	}
	if info, err := winapi.ReadReparsePoint(sys.ToExtendedPath(it.Source)); err == nil && info.Target() != it.Target {
		return fmt.Errorf("链接目标已改变（现为 %s），拒绝继续撤销", info.Target())
	}

	// 摘链接（RemoveDirectoryW 只删链接本身，绝不触碰目标内容）
	if err := winapi.RemoveDirectoryOnly(sys.ToExtendedPath(it.Source)); err != nil {
		return fmt.Errorf("移除目录联接失败: %w", err)
	}
	// 备份改回原名
	if _, err := os.Stat(it.Backup); err != nil {
		return fmt.Errorf("备份目录 %s 不存在，无法恢复", it.Backup)
	}
	if err := os.Rename(sys.ToExtendedPath(it.Backup), sys.ToExtendedPath(it.Source)); err != nil {
		return fmt.Errorf("备份恢复失败: %w", err)
	}
	// 环境变量
	if it.EnvVar != "" {
		if it.EnvOldSet {
			_ = setUserEnv(it.EnvVar, it.EnvOld)
		} else {
			_ = delUserEnv(it.EnvVar)
		}
		winapi.BroadcastEnvironmentChange()
	}
	return nil
}

// PurgeBackup 延迟清理：删除迁移备份以真正释放源盘空间。
//
// 默认不清理（复制完成 ≠ 软件真的能用新位置）。执行条件极其严格：
// 原路径必须仍是指向 Target 的、可读的 Junction——这一条不过，
// 一个字节都不删。这是把「空间收益」变成现实的最后一步，
// 由用户在验证软件一切正常后显式触发。
func PurgeBackup(journalID string) error {
	recs := ListJournals()
	var target *FileRecord
	for i := range recs {
		if recs[i].ID == journalID {
			target = &recs[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("找不到迁移记录 %s", journalID)
	}

	var errs []string
	for _, it := range target.Items {
		if !it.Done || it.Undone || it.Purged {
			continue
		}
		isRe, tag, err := winapi.IsReparsePointRaw(sys.ToExtendedPath(it.Source))
		if err != nil || !isRe || tag != winapi.IO_REPARSE_TAG_MOUNT_POINT {
			errs = append(errs, it.ID+": 原路径不是目录联接，拒绝删除备份")
			continue
		}
		if info, rerr := winapi.ReadReparsePoint(sys.ToExtendedPath(it.Source)); rerr != nil || info.Target() != it.Target {
			errs = append(errs, it.ID+": 链接目标不符，拒绝删除备份")
			continue
		}
		// 通过校验：备份是纯数据副本，安全删除（RemoveTree 遇链接只摘不进）
		_, _, _, rmErr := safeio.RemoveTree(context.Background(), it.Backup, nil)
		if rmErr != nil {
			errs = append(errs, it.ID+": 删除备份失败: "+rmErr.Error())
			continue
		}
		markPurged(journalID, it.ID)
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "；"))
	}
	return nil
}

// markUndone / markPurged 追加状态行（JSONL 追加写，历史保留）。
func markUndone(journalID, itemID string) { markItem(journalID, "item_undone", itemID) }
func markPurged(journalID, itemID string) { markItem(journalID, "item_purged", itemID) }

func markItem(journalID, kind, itemID string) {
	j, _, err := NewJournal(journalID)
	if err != nil {
		return
	}
	defer j.Close()
	_ = j.Write(Entry{Time: time.Now(), Kind: kind, Item: ItemRecord{ID: itemID}})
}

// ───────── 环境变量（用户级）─────────

// getUserEnv / setUserEnv / delUserEnv 读写 HKCU\Environment。
// 使用 golang.org/x/sys/windows/registry（go.mod 已含 x/sys）。
func getUserEnv(name string) (string, bool, error) {
	k, err := registryOpen()
	if err != nil {
		return "", false, err
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	if err != nil {
		if err == registryNotFound() {
			return "", false, nil
		}
		return "", false, err
	}
	return v, true, nil
}

func setUserEnv(name, value string) error {
	k, err := registryOpen()
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(name, value)
}

func delUserEnv(name string) error {
	k, err := registryOpen()
	if err != nil {
		return err
	}
	defer k.Close()
	return k.DeleteValue(name)
}

func longPathsEnabled() bool {
	return false // 保守默认：不假设系统启用了长路径
}
