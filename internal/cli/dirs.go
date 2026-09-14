package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"winclean/internal/model"
	"winclean/internal/report"
	"winclean/internal/scan"
	"winclean/internal/sys"
)

// cmdDirs 从已有的 scan.json 读取并列出目录占用。
//
// 刻意要求显式提供输入文件而不自动重新扫描：扫描是 O(文件数) 的重操作，
// 让用户能在一份结果上反复用不同条件查看，是开发与排查效率的关键。
func cmdDirs(args []string) int {
	fs := flag.NewFlagSet("dirs", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { usage(os.Stderr) }

	input := "winclean-scan.json"
	fs.StringVar(&input, "i", input, "scan.json 路径")
	fs.StringVar(&input, "input", input, "scan.json 路径")

	var (
		top      = fs.Int("top", 30, "显示条数（0 表示全部）")
		minSize  = fs.String("min-size", "", "只显示不小于该体积的目录")
		maxDepth = fs.Int("depth", -1, "只显示不超过该层级的目录（-1 表示不限）")
		volume   = fs.String("volume", "", "只显示指定盘（如 C）")
		onlyLink = fs.Bool("reparse", false, "只显示重解析点（链接）")
		asJSON   = fs.Bool("json", false, "以 JSON 输出")
	)

	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}

	res, err := report.ReadJSON(input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return ExitNoScanFile
	}

	var minBytes int64
	if *minSize != "" {
		n, err := scan.ParseSize(*minSize)
		if err != nil {
			fmt.Fprintf(os.Stderr, "无效的 --min-size：%v\n", err)
			return ExitUsage
		}
		minBytes = n
	}

	sel := filterDirs(res.Dirs, dirFilter{
		MinSize:  minBytes,
		MaxDepth: *maxDepth,
		Volume:   strings.ToUpper(*volume),
		OnlyLink: *onlyLink,
	})

	if *top > 0 && len(sel) > *top {
		sel = sel[:*top]
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(sel); err != nil {
			fmt.Fprintf(os.Stderr, "输出失败：%v\n", err)
			return ExitError
		}
		return ExitOK
	}

	fmt.Printf("来源 %s（扫描于 %s，模式 %s）\n",
		input, res.ScannedAt.Format("2006-01-02 15:04:05"), res.Mode)
	if len(sel) == 0 {
		fmt.Println("\n没有符合条件的结果。可放宽 --min-size 或 --depth 后重试。")
		return ExitOK
	}

	report.Section(os.Stdout, fmt.Sprintf("目录占用（%d 条）", len(sel)))
	t := report.New([]string{"占用", "逻辑大小", "文件数", "深度", "路径"},
		true, true, true, true, false)
	for _, d := range sel {
		path := d.Path
		switch {
		case d.IsReparse:
			target := d.LinkTarget
			if target == "" {
				target = "?"
			}
			path = fmt.Sprintf("%s  → %s [%s]", d.Path, target, d.ReparseTag)
		case d.SummaryOnly:
			path += "  [仅汇总]"
		case d.Skipped:
			path += "  [未读全：" + d.SkipReason + "]"
		}
		t.Add(report.Bytes(d.OnDisk), report.Bytes(d.Logical), report.Count(d.Files),
			fmt.Sprintf("%d", d.Depth), path)
	}
	t.Render(os.Stdout)

	// 合计只对顶层条目求和，避免父与子重复累加
	var total int64
	for _, d := range res.Dirs {
		if d.Depth == 0 {
			total += d.OnDisk
		}
	}
	if total > 0 {
		fmt.Printf("\n扫描根合计：%s\n", report.Bytes(total))
	}
	return ExitOK
}

// dirFilter 是 dirs 命令的筛选条件。
type dirFilter struct {
	MinSize  int64
	MaxDepth int
	Volume   string
	OnlyLink bool
}

func filterDirs(dirs []model.DirEntry, f dirFilter) []model.DirEntry {
	out := make([]model.DirEntry, 0, len(dirs))
	for _, d := range dirs {
		if f.OnlyLink && !d.IsReparse {
			continue
		}
		if f.MinSize > 0 && d.OnDisk < f.MinSize {
			continue
		}
		if f.MaxDepth >= 0 && d.Depth > f.MaxDepth {
			continue
		}
		if f.Volume != "" && sys.VolumeOf(d.Path) != f.Volume {
			continue
		}
		// --reparse 模式下链接本身占用为 0，不该被 min-size 过滤掉
		if f.MinSize > 0 && d.IsReparse && d.OnDisk < f.MinSize {
			out = append(out, d)
			continue
		}
		out = append(out, d)
	}
	return out
}
