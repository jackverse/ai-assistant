package report

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strings"
	"time"

	"winclean/internal/model"
)

// 模板随二进制分发（embed），因此 exe 单文件即可生成完整报告，
// 不需要在旁边放模板文件。
//
//go:embed templates/report.html
var reportHTML string

var htmlTemplate = template.Must(template.New("report").Parse(reportHTML))

// htmlView 是 HTML 报告的渲染模型。
type htmlView struct {
	ToolVersion string
	GeneratedAt string
	ScannedAt   string
	Duration    string
	Mode        string
	Roots       string
	Warnings    []string

	Volumes []htmlVolume
	Dirs    []htmlRow
	Files   []htmlRow
	Stats   []htmlKV

	TotalOnDisk   string
	TotalLogical  string
	ScannedFiles  string
	ScannedDirs   string
	SkippedDirs   int64
	TopDirBytes   int64
	MaxFileBytes  int64
	DirCount      int
	FileCount     int
	HardLinkNote  string
	VolumeUsed    string
	CoverageNote  string
}

type htmlVolume struct {
	Letter     string
	Label      string
	FileSystem string
	Total      string
	Used       string
	Free       string
	UsedPct    string
	UsedPctNum float64
	IsSystem   bool
	Critical   bool
}

type htmlRow struct {
	Size      string
	Logical   string
	Count     string
	Depth     string
	Path      string
	Badge     string
	BadgeKind string
	BarPct    float64
	SortSize  int64
	SortCount int64
	SortDepth int
}

type htmlKV struct {
	Key   string
	Value string
}

// WriteHTML 生成 HTML 报告并写入文件。
//
// 设计约束（重要）：**不引用任何外部资源**——不加载 CDN 的 CSS/JS/字体。
// 原因有二：① 本工具的网络环境可能访问不到境外 CDN；② 报告里含用户的
// 完整目录结构，不应因为打开报告而向任何服务器发起请求。
func WriteHTML(path string, res *model.ScanResult, maxDirs int) error {
	view := buildHTMLView(res, maxDirs)

	var buf bytes.Buffer
	if err := htmlTemplate.Execute(&buf, view); err != nil {
		return fmt.Errorf("渲染 HTML 失败: %w", err)
	}

	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("替换 %s 失败: %w", path, err)
	}
	return nil
}

func buildHTMLView(res *model.ScanResult, maxDirs int) htmlView {
	v := htmlView{
		ToolVersion: res.ToolVersion,
		GeneratedAt: time.Now().Format("2006-01-02 15:04:05"),
		ScannedAt:   res.ScannedAt.Format("2006-01-02 15:04:05"),
		Duration:    Duration(time.Duration(res.DurationMS) * time.Millisecond),
		Mode:        modeLabel(res.Mode),
		Roots:       strings.Join(res.Roots, ", "),
		Warnings:    res.Warnings,
	}

	for _, vol := range res.Volumes {
		label := vol.Label
		if label == "" {
			label = "—"
		}
		hv := htmlVolume{
			Letter:     vol.Letter + ":",
			Label:      label,
			FileSystem: vol.FileSystem,
			Total:      Bytes(int64(vol.TotalBytes)),
			Used:       Bytes(int64(vol.UsedBytes)),
			Free:       Bytes(int64(vol.FreeBytes)),
			UsedPct:    fmt.Sprintf("%.1f%%", vol.UsedPercent()),
			UsedPctNum: vol.UsedPercent(),
			IsSystem:   vol.IsSystem,
			Critical:   vol.TotalBytes > 0 && vol.UsedPercent() > 90,
		}
		v.Volumes = append(v.Volumes, hv)
	}

	// 目录表：以最大占用为 100% 画条
	dirs := res.Dirs
	if maxDirs > 0 && len(dirs) > maxDirs {
		dirs = dirs[:maxDirs]
	}
	for _, d := range dirs {
		if d.OnDisk > v.TopDirBytes {
			v.TopDirBytes = d.OnDisk
		}
	}
	for _, d := range dirs {
		badge, kind := "", ""
		switch {
		case d.IsReparse:
			badge = "链接 " + d.ReparseTag
			kind = "badge-link"
		case d.SummaryOnly:
			badge = "仅汇总"
			kind = "badge-summary"
		case d.Skipped:
			badge = "未读全"
			kind = "badge-warn"
		}
		row := htmlRow{
			Size:      Bytes(d.OnDisk),
			Logical:   Bytes(d.Logical),
			Count:     Count(d.Files),
			Depth:     fmt.Sprintf("%d", d.Depth),
			Path:      d.Path,
			Badge:     badge,
			BadgeKind: kind,
			SortSize:  d.OnDisk,
			SortCount: d.Files,
			SortDepth: d.Depth,
		}
		if v.TopDirBytes > 0 {
			row.BarPct = float64(d.OnDisk) / float64(v.TopDirBytes) * 100
		}
		if d.IsReparse && d.LinkTarget != "" {
			row.Path = d.Path + "  →  " + d.LinkTarget
		}
		v.Dirs = append(v.Dirs, row)
	}
	v.DirCount = len(dirs)

	for _, f := range res.LargestFiles {
		if f.OnDisk > v.MaxFileBytes {
			v.MaxFileBytes = f.OnDisk
		}
	}
	for _, f := range res.LargestFiles {
		mt := "—"
		if f.Mtime != nil {
			mt = f.Mtime.Format("2006-01-02")
		}
		row := htmlRow{
			Size:      Bytes(f.OnDisk),
			Logical:   Bytes(f.Logical),
			Count:     mt,
			Depth:     "—",
			Path:      f.Path,
			SortSize:  f.OnDisk,
			SortCount: 0,
			SortDepth: 0,
		}
		if v.MaxFileBytes > 0 {
			row.BarPct = float64(f.OnDisk) / float64(v.MaxFileBytes) * 100
		}
		if f.IsReparse {
			row.Badge, row.BadgeKind = "重解析点", "badge-link"
		}
		v.Files = append(v.Files, row)
	}
	v.FileCount = len(res.LargestFiles)

	s := res.Stats
	v.TotalOnDisk = Bytes(s.BytesOnDisk)
	v.TotalLogical = Bytes(s.BytesLogical)
	v.ScannedFiles = Count(s.FilesScanned)
	v.ScannedDirs = Count(s.DirsScanned)
	v.SkippedDirs = s.SkippedDirs

	v.Stats = []htmlKV{
		{"扫描文件数", Count(s.FilesScanned)},
		{"扫描目录数", Count(s.DirsScanned)},
		{"占用（实际）", Bytes(s.BytesOnDisk)},
		{"占用（逻辑）", Bytes(s.BytesLogical)},
		{"重解析点", Count(s.ReparsePoints)},
		{"最大深度", fmt.Sprintf("%d", s.MaxDepthReached)},
	}
	if s.HardLinkDedup {
		v.Stats = append(v.Stats, htmlKV{"硬链接去重",
			fmt.Sprintf("%s 个（该范围共 %s 个文件含硬链接）",
				Count(s.HardLinksDeduped), Count(s.HardLinkFiles))})
		v.HardLinkNote = "深度模式：已按文件索引去重硬链接"
	} else {
		v.Stats = append(v.Stats, htmlKV{"硬链接去重", "未启用（快速模式）"})
		v.HardLinkNote = "快速模式未去重硬链接：NTFS 硬链接会被重复计入，" +
			"C:\\Windows 这类目录的体积会偏高。用 --deep 可得到精确值"
	}
	if s.CloudPlaceholders > 0 {
		v.Stats = append(v.Stats, htmlKV{"云占位/稀疏文件", Count(s.CloudPlaceholders)})
	}
	if s.SkippedDirs > 0 {
		v.Stats = append(v.Stats, htmlKV{"跳过目录（权限）", Count(s.SkippedDirs)})
	}
	if s.Errors > 0 {
		v.Stats = append(v.Stats, htmlKV{"读取错误", Count(s.Errors)})
	}
	if s.VolumeUsedBytes > 0 {
		v.VolumeUsed = Bytes(int64(s.VolumeUsedBytes))
		diff := float64(int64(s.VolumeUsedBytes)-s.BytesOnDisk) / float64(s.VolumeUsedBytes) * 100
		v.CoverageNote = fmt.Sprintf(
			"扫描统计 %s，卷实际已用 %s，相差 %.1f%%。注意两个方向的误差会互相抵消："+
				"硬链接不去重与簇对齐会高估，权限不足跳过的区域会低估，"+
				"因此「数字接近」不代表覆盖完整（有 %d 个目录未读到）",
			Bytes(s.BytesOnDisk), Bytes(int64(s.VolumeUsedBytes)), diff, s.SkippedDirs)
	}

	if len(s.SkippedReasons) > 0 {
		var b strings.Builder
		n := len(s.SkippedReasons)
		if n > 15 {
			n = 15
		}
		for _, sr := range s.SkippedReasons[:n] {
			fmt.Fprintf(&b, "%s — %s\n", sr.Path, sr.Reason)
		}
		if len(s.SkippedReasons) > n {
			fmt.Fprintf(&b, "…… 其余 %d 条见 scan.json 的 stats.skipped_reasons\n",
				len(s.SkippedReasons)-n)
		}
		v.Stats = append(v.Stats, htmlKV{"跳过明细（前 15 条）", b.String()})
	}

	return v
}

func modeLabel(m model.ScanMode) string {
	switch m {
	case model.ModeDeep:
		return "深度模式（真实磁盘占用 + 硬链接去重）"
	case model.ModeTargeted:
		return "定点扫描"
	default:
		return "快速模式（按簇对齐估算）"
	}
}

// MarshalJSONView 输出报告视图的 JSON（便于脚本消费）。
func MarshalJSONView(res *model.ScanResult, maxDirs int) ([]byte, error) {
	v := buildHTMLView(res, maxDirs)
	return json.MarshalIndent(v, "", "  ")
}
