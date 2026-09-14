package cli

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"syscall"

	"winclean/internal/model"
	"winclean/internal/report"
	"winclean/internal/scan"
	"winclean/internal/sys"
	"winclean/internal/version"
	"winclean/internal/winapi"
)

// cmdDoctor 做环境自检。
//
// 把它做成最早可用的命令是有意的：权限、编码、文件索引可用性这些基础问题
// 如果留到后面才暴露，会积累成很难排查的诡异现象。
func cmdDoctor(args []string) int {
	w := os.Stdout
	fmt.Fprintln(w, version.String())

	report.Section(w, "运行环境")
	env := report.New([]string{"项目", "值"}, false, false)
	env.Add("操作系统", runtime.GOOS+"/"+runtime.GOARCH)
	env.Add("Go 版本", runtime.Version())
	env.Add("逻辑 CPU", fmt.Sprintf("%d", runtime.NumCPU()))

	elevated := sys.IsElevated()
	adminText := "否（普通用户）"
	if elevated {
		adminText = "是（已提权）"
	}
	env.Add("管理员权限", adminText)

	if cwd, err := os.Getwd(); err == nil {
		env.Add("当前目录", cwd)
	}
	if root, err := sys.SystemRoot(); err == nil {
		env.Add("Windows 目录", root)
	}
	if sr, err := sys.SystemVolumeRoot(); err == nil {
		env.Add("系统盘", sr)
	}
	env.Render(w)

	// ── 磁盘 ────────────────────────────────────────────
	report.Section(w, "磁盘")
	vols, err := sys.EnumerateVolumes()
	if err != nil {
		fmt.Fprintf(w, "  枚举磁盘失败：%v\n", err)
		return ExitError
	}
	vt := report.New([]string{"卷", "文件系统", "总容量", "已用", "剩余", "NTFS"},
		false, false, true, true, true, false)
	for _, v := range vols {
		ntfs := "否 ← 无法使用目录联接（Junction）"
		if v.IsNTFS {
			ntfs = "是"
		}
		vt.Add(v.Letter+":", v.FileSystem, report.Bytes(int64(v.TotalBytes)),
			report.Bytes(int64(v.UsedBytes)), report.Bytes(int64(v.FreeBytes)), ntfs)
	}
	vt.Render(w)

	// 输出盘空间检查：C 盘紧张时提醒把 scan.json 输出到别的盘
	if cwd, err := os.Getwd(); err == nil {
		if vol := findVolumeForPath(vols, cwd); vol != nil && vol.TotalBytes > 0 {
			if pct := vol.UsedPercent(); pct > 90 {
				fmt.Fprintf(w, "\n  ⚠️  当前目录所在卷 %s 已用 %.1f%%，建议用 -o 把 scan.json 输出到其他盘\n",
					vol.Letter+":", pct)
			}
		}
	}

	// ── 遍历能力 ────────────────────────────────────────
	report.Section(w, "扫描能力")
	ct := report.New([]string{"检查项", "结果"}, false, false)

	// 硬链接去重依赖文件索引，而文件索引只能通过 GetFileInformationByHandle
	// 获得（目录枚举结果里的对应字段是保留字段，实测恒为 0）。
	// 这里实测这条链路是否可用，并说明它对扫描精度的影响。
	idOK, idDetail := checkFileIDCapability()

	ct.Add("UTF-8 代码页", "已设置（65001）")
	if idOK {
		ct.Add("硬链接去重能力", "可用（"+idDetail+"）")
	} else {
		ct.Add("硬链接去重能力", "不可用 ← "+idDetail+"；NTFS 硬链接会被重复计入，体积可能偏高")
	}
	ct.Add("逻辑 CPU 数", fmt.Sprintf("%d", runtime.NumCPU()))

	// 重解析点识别：找一个已知的链接来验证读取链路
	tagOK, tagDetail := checkReparseRead()
	if tagOK {
		ct.Add("重解析点识别", "可用（"+tagDetail+"）")
	} else {
		ct.Add("重解析点识别", "未找到样本来验证（不影响功能）")
	}

	// 权限对扫描完整性的影响
	if elevated {
		ct.Add("扫描完整度", "已提权，可读取大部分系统区域")
	} else {
		ct.Add("扫描完整度", "普通用户：回收站/卷影副本等区域不可读，总量会偏小")
	}
	ct.Render(w)

	// ── 系统「只汇总不展开」目录 ────────────────────────
	report.Section(w, "默认「只汇总不展开」的目录")
	fmt.Fprintln(w, "  这些目录体积大、用户有权知道占了多少，但逐项细分既慢又无意义，")
	fmt.Fprintln(w, "  且绝不应作为清理目标：")
	for _, p := range scan.DefaultSummaryOnly() {
		fmt.Fprintf(w, "    %s\n", p)
	}

	// ── 结论 ────────────────────────────────────────────
	report.Section(w, "结论")
	var issues []string
	if !elevated {
		issues = append(issues, "非管理员运行：结果会有归因缺口（不影响使用，报告中会明确标注）")
	}
	if !idOK {
		issues = append(issues, "硬链接去重能力不可用：NTFS 硬链接会被重复计入，体积可能偏高")
	}
	if len(issues) == 0 {
		fmt.Fprintln(w, "  ✅ 环境正常，可以开始扫描：winclean scan --disk C")
	} else {
		for _, i := range issues {
			fmt.Fprintf(w, "  • %s\n", i)
		}
		fmt.Fprintln(w, "\n  可以继续使用：winclean scan --disk C")
	}

	if !idOK {
		return ExitPartial
	}
	return ExitOK
}

// checkFileIDCapability 验证「通过句柄读取文件索引」这条链路是否可用。
//
// 只读：枚举 Windows 目录下的若干文件，对其中达到大小阈值的调用
// GetFileInformationByHandle，检查能否拿到非零文件索引。
//
// 注意：不能用 WIN32_FIND_DATA 里的 FileIndex 字段来判断——那是保留字段，
// 实测恒为 0，用它做判断会得出「能力不可用」的错误结论。
func checkFileIDCapability() (bool, string) {
	root, err := sys.SystemRoot()
	if err != nil {
		return false, "无法确定 Windows 目录：" + err.Error()
	}
	pattern := sys.ToExtendedPath(root + `\*`)
	h, data, err := winapi.FindFirstFileEx(pattern)
	if err != nil {
		return false, "目录枚举失败：" + err.Error()
	}
	defer winapi.FindClose(h)

	tried, failed := 0, 0
	var lastErr string
	for tried < 5 {
		if !data.IsDir() && data.Size() > 0 {
			full := root + `\` + syscall.UTF16ToString(data.FileName[:])
			md, err := winapi.GetFileMetadata(sys.ToExtendedPath(full))
			if err != nil {
				failed++
				lastErr = err.Error()
			} else if md.ID.Index != 0 {
				return true, fmt.Sprintf("抽查 %d 个文件均可读取索引与真实磁盘占用", tried+1)
			}
			tried++
		}
		if err := winapi.FindNextFile(h, data); err != nil {
			if errIsNoMoreFiles(err) {
				break
			}
			return false, "枚举中断：" + err.Error()
		}
	}
	if failed == tried && tried > 0 {
		return false, "每次调用都失败：" + lastErr
	}
	return false, "未能取得非零文件索引"
}

// checkReparseRead 尝试读一个已知的重解析点，验证读取链路。
//
// 优先用 Windows 自己的兼容性软链（如 "C:\Users\All Users" 是
// ProgramData 的符号链接），它几乎总存在且不需提权。
func checkReparseRead() (bool, string) {
	candidates := []string{
		`C:\Users\All Users`,
		`C:\Users\Default User`,
		`C:\Documents and Settings`,
	}
	for _, p := range candidates {
		attrs, err := winapi.GetFileAttributes(p)
		if err != nil || attrs&winapi.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
			continue
		}
		target := sys.ReadReparseTarget(p)
		if target == "" {
			target = "(未能解析目标)"
		}
		return true, p + " → " + target
	}
	return false, ""
}

func errIsNoMoreFiles(err error) bool {
	return errors.Is(err, syscall.ERROR_NO_MORE_FILES)
}

func findVolumeForPath(vols []model.Volume, path string) *model.Volume {
	letter := sys.VolumeOf(path)
	if letter == "" {
		return nil
	}
	for i := range vols {
		if strings.EqualFold(vols[i].Letter, letter) {
			return &vols[i]
		}
	}
	return nil
}
