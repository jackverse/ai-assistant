package report

import (
	"fmt"
	"io"
	"strings"
	"time"

	"winclean/internal/model"
)

// Table 是一个按【终端显示宽度】对齐的表格。
//
// 为什么不直接用 text/tabwriter：tabwriter 按 rune 数计算列宽，
// 而中文是全角字符（占 2 列），导致含中文的列全部错位。
// 本工具的报告大量使用中文表头与中文路径，必须按显示宽度对齐。
type Table struct {
	header  []string
	rows    [][]string
	rights  []bool
}

// New 创建一个表格，header 为表头，rights 标记哪些列右对齐。
func New(header []string, rights ...bool) *Table {
	r := make([]bool, len(header))
	copy(r, rights)
	return &Table{header: header, rights: r}
}

// Add 追加一行。
func (t *Table) Add(cells ...string) {
	t.rows = append(t.rows, cells)
}

// Render 输出表格。
func (t *Table) Render(w io.Writer) {
	widths := make([]int, len(t.header))
	for i, h := range t.header {
		widths[i] = displayWidth(h)
	}
	for _, row := range t.rows {
		for i, c := range row {
			if i < len(widths) {
				if d := displayWidth(c); d > widths[i] {
					widths[i] = d
				}
			}
		}
	}

	writeRow := func(cells []string) {
		var parts []string
		for i, c := range cells {
			if i >= len(widths) {
				break
			}
			right := i < len(t.rights) && t.rights[i]
			parts = append(parts, pad(c, widths[i], right))
		}
		fmt.Fprintln(w, strings.TrimRight(strings.Join(parts, "  "), " "))
	}

	writeRow(t.header)
	sep := make([]string, len(t.header))
	for i := range sep {
		sep[i] = strings.Repeat("─", widths[i])
	}
	writeRow(sep)
	for _, row := range t.rows {
		writeRow(row)
	}
}

// pad 按显示宽度补空格。
func pad(s string, width int, right bool) string {
	d := displayWidth(s)
	if d >= width {
		return s
	}
	spaces := strings.Repeat(" ", width-d)
	if right {
		return spaces + s
	}
	return s + spaces
}

// ScanTable 渲染扫描结果概览。
//
// 输出顺序刻意如此：先磁盘总量，再目录占用，最后单个大文件。
// 因为「C 盘为什么满了」的答案往往既在目录层（AppData），
// 也在单个文件层（docker_data.vhdx、pagefile.sys），两层都必须呈现。
func ScanTable(w io.Writer, res *model.ScanResult, topDirs, topFiles int) {
	fmt.Fprintf(w, "winclean %s · 扫描完成\n", res.ToolVersion)
	fmt.Fprintf(w, "模式 %s   耗时 %s   扫描根 %s\n",
		describeMode(res.Mode), Duration(time.Duration(res.DurationMS)*time.Millisecond),
		strings.Join(res.Roots, ", "))

	// ── 磁盘 ────────────────────────────────────────────
	Section(w, "磁盘")
	vt := New([]string{"卷", "卷标", "文件系统", "总容量", "已用", "剩余", "使用率"},
		false, false, false, true, true, true, true)
	for _, v := range res.Volumes {
		label := v.Label
		if label == "" {
			label = "-"
		}
		flag := ""
		if v.IsSystem {
			flag = " ←系统盘"
		}
		pct := fmt.Sprintf("%.1f%%", v.UsedPercent())
		if v.TotalBytes > 0 && v.UsedPercent() > 90 {
			pct += " ⚠️"
		}
		vt.Add(v.Letter+":", label, v.FileSystem, Bytes(int64(v.TotalBytes)),
			Bytes(int64(v.UsedBytes)), Bytes(int64(v.FreeBytes)), pct+flag)
	}
	vt.Render(w)

	// ── 目录占用 ────────────────────────────────────────
	dirs := res.Dirs
	if topDirs > 0 && len(dirs) > topDirs {
		dirs = dirs[:topDirs]
	}
	if len(dirs) > 0 {
		Section(w, fmt.Sprintf("目录占用 Top %d", len(dirs)))
		dt := New([]string{"占用", "逻辑大小", "文件数", "深度", "路径"},
			true, true, true, true, false)
		for _, d := range dirs {
			path := d.Path
			if d.IsReparse {
				target := d.LinkTarget
				if target == "" {
					target = "?"
				}
				path = fmt.Sprintf("%s  → %s [链接]", d.Path, target)
			}
			if d.SummaryOnly {
				path += "  [仅汇总]"
			}
			dt.Add(Bytes(d.OnDisk), Bytes(d.Logical), Count(d.Files),
				fmt.Sprintf("%d", d.Depth), path)
		}
		dt.Render(w)
	}

	// ── 最大文件 ────────────────────────────────────────
	files := res.LargestFiles
	if topFiles > 0 && len(files) > topFiles {
		files = files[:topFiles]
	}
	if len(files) > 0 {
		Section(w, fmt.Sprintf("最大文件 Top %d", len(files)))
		ft := New([]string{"占用", "逻辑大小", "修改时间", "路径"}, true, true, false, false)
		for _, f := range files {
			mt := "-"
			if f.Mtime != nil {
				mt = f.Mtime.Format("2006-01-02")
			}
			note := ""
			if f.IsReparse {
				note = " [重解析点]"
			}
			ft.Add(Bytes(f.OnDisk), Bytes(f.Logical), mt, f.Path+note)
		}
		ft.Render(w)
	}

	// ── 统计 ────────────────────────────────────────────
	Section(w, "统计")
	s := res.Stats
	st := New([]string{"项目", "数值"}, false, true)
	st.Add("扫描文件数", Count(s.FilesScanned))
	st.Add("扫描目录数", Count(s.DirsScanned))
	st.Add("占用（实际）", Bytes(s.BytesOnDisk))
	st.Add("占用（逻辑）", Bytes(s.BytesLogical))
	if s.VolumeUsedBytes > 0 {
		gap := float64(int64(s.VolumeUsedBytes)-s.BytesOnDisk) / float64(s.VolumeUsedBytes) * 100
		st.Add("卷实际已用", fmt.Sprintf("%s（统计值与它相差 %.1f%%，见下方提示）",
			Bytes(int64(s.VolumeUsedBytes)), gap))
	}
	st.Add("重解析点", Count(s.ReparsePoints))
	if s.HardLinkDedup {
		if s.HardLinksDeduped > 0 {
			st.Add("硬链接去重", fmt.Sprintf("%s 个（该目录树共 %s 个文件含硬链接）",
				Count(s.HardLinksDeduped), Count(s.HardLinkFiles)))
		} else {
			st.Add("硬链接去重", "已启用，未发现硬链接")
		}
	} else {
		st.Add("硬链接去重", "未启用（快速模式）← NTFS 硬链接会被重复计入")
	}
	if s.CloudPlaceholders > 0 {
		st.Add("云占位/稀疏文件", Count(s.CloudPlaceholders))
	}
	st.Add("最大深度", fmt.Sprintf("%d", s.MaxDepthReached))
	if s.SkippedDirs > 0 {
		st.Add("跳过目录（权限）", Count(s.SkippedDirs))
	}
	if s.Errors > 0 {
		st.Add("读取错误", Count(s.Errors))
	}
	st.Render(w)

	// ── 跳过明细 ────────────────────────────────────────
	if len(s.SkippedReasons) > 0 {
		Section(w, "被跳过的目录（前 10 条）")
		n := len(s.SkippedReasons)
		if n > 10 {
			n = 10
		}
		for _, sr := range s.SkippedReasons[:n] {
			fmt.Fprintf(w, "  %s\n    原因：%s\n", sr.Path, sr.Reason)
		}
		if len(s.SkippedReasons) > n {
			fmt.Fprintf(w, "  …… 其余 %d 条见 JSON 输出的 stats.skipped_reasons\n",
				len(s.SkippedReasons)-n)
		}
	}

	// ── 告警 ────────────────────────────────────────────
	if len(res.Warnings) > 0 {
		Section(w, "提示")
		for _, warn := range res.Warnings {
			fmt.Fprintf(w, "  ⚠️  %s\n", warn)
		}
	}
}

func describeMode(m model.ScanMode) string {
	switch m {
	case model.ModeDeep:
		return "deep（深度：读取重解析点详情与真实磁盘占用）"
	case model.ModeTargeted:
		return "targeted（定点扫描）"
	default:
		return "fast（快速：按簇对齐估算占用）"
	}
}
