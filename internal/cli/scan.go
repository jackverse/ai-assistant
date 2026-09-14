package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"winclean/internal/report"
	"winclean/internal/scan"
)

// stringList 是可重复出现的字符串参数（如 --exclude 可给多次）。
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("值不能为空")
	}
	*s = append(*s, v)
	return nil
}

func cmdScan(args []string) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { usage(os.Stderr) }

	var (
		disk     string
		paths    stringList
		excludes stringList
		summary  stringList

		maxDepth = fs.Int("max-depth", 0, "上报的目录层级上限")
		minSize  = fs.String("min-size", "", "只上报不小于该体积的目录，如 10MB")
		workers  = fs.Int("workers", 0, "并发数")
		deep     = fs.Bool("deep", false, "深度模式")
		format   = fs.String("format", "table", "输出格式 table|json")
		noSave   = fs.Bool("no-save", false, "不写 scan.json")
		topFiles = fs.Int("top-files", scan.DefaultTopFiles, "收集的最大文件数")
		topDirs  = fs.Int("top-dirs", 20, "报告中显示的目录条数")
		quiet    = fs.Bool("quiet", false, "不显示进度")
		htmlOut  = fs.String("html", "", "同时生成 HTML 报告到该路径")
		openHTML = fs.Bool("open", false, "生成 HTML 后用浏览器打开")
	)
	output := "winclean-scan.json"
	fs.StringVar(&output, "o", output, "scan.json 输出路径")
	fs.StringVar(&output, "output", output, "scan.json 输出路径")
	fs.StringVar(&disk, "disk", "", "要扫描的盘，如 C 或 C,D")
	fs.Var(&paths, "path", "定点扫描目录（可重复）")
	fs.Var(&excludes, "exclude", "完全跳过的目录（可重复）")
	fs.Var(&summary, "summary-only", "只汇总不展开的目录（可重复）")
	fs.BoolVar(quiet, "q", false, "不显示进度")

	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}

	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "多余的参数：%v\n", fs.Args())
		return ExitUsage
	}
	if *format != "table" && *format != "json" {
		fmt.Fprintf(os.Stderr, "无效的 --format：%s（只支持 table 或 json）\n", *format)
		return ExitUsage
	}

	// 组装扫描根：--path 优先于 --disk，两者都没有时用系统盘
	var roots []string
	if len(paths) > 0 {
		roots = append(roots, paths...)
	} else if disk != "" {
		for _, d := range strings.Split(disk, ",") {
			d = strings.TrimSpace(d)
			if d == "" {
				continue
			}
			d = strings.TrimSuffix(d, ":")
			if len(d) != 1 {
				fmt.Fprintf(os.Stderr, "无效的盘符：%q\n", d)
				return ExitUsage
			}
			roots = append(roots, strings.ToUpper(d)+`:\\`)
		}
	}

	opts := scan.Options{
		Roots:       roots,
		MaxDepth:    *maxDepth,
		Deep:        *deep,
		Workers:     *workers,
		Excludes:    excludes,
		SummaryOnly: summary,
		TopFiles:    *topFiles,
	}
	if *minSize != "" {
		n, err := scan.ParseSize(*minSize)
		if err != nil {
			fmt.Fprintf(os.Stderr, "无效的 --min-size：%v\n", err)
			return ExitUsage
		}
		opts.MinSize = n
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// 进度条用一个轻量 goroutine 渲染到 stderr，
	// 绝不写 stdout —— stdout 可能被重定向为 JSON。
	prog := scan.NewProgress()
	opts.Progress = prog

	start := time.Now()
	stopProgress := func() {}
	if !*quiet && isTerminal(os.Stderr) {
		stopProgress = startProgressRenderer(os.Stderr, prog, start)
	}

	res, err := scan.Run(ctx, opts)
	stopProgress()
	if err != nil {
		fmt.Fprintf(os.Stderr, "扫描失败：%v\n", err)
		return ExitError
	}

	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "\n已中断。以下是中断前已完成的部分结果。")
	}

	// 输出
	switch *format {
	case "json":
		data, err := report.MarshalJSON(res)
		if err != nil {
			fmt.Fprintf(os.Stderr, "序列化失败：%v\n", err)
			return ExitError
		}
		os.Stdout.Write(data)
	default:
		report.ScanTable(os.Stdout, res, *topDirs, 15)
	}

	if !*noSave {
		if err := report.WriteJSON(output, res); err != nil {
			fmt.Fprintf(os.Stderr, "写入 %s 失败：%v\n", output, err)
			return ExitError
		}
		if *format != "json" {
			fmt.Printf("\n完整结果已写入 %s\n", output)
		}
	}

	// HTML 报告：--open 单独出现时也要能用，所以补一个默认路径。
	htmlPath := *htmlOut
	if htmlPath == "" && *openHTML {
		htmlPath = "winclean-report.html"
	}
	if htmlPath != "" {
		if err := report.WriteHTML(htmlPath, res, 300); err != nil {
			fmt.Fprintf(os.Stderr, "生成 HTML 报告失败：%v\n", err)
			return ExitError
		}
		abs, _ := filepath.Abs(htmlPath)
		fmt.Printf("HTML 报告已写入 %s\n", abs)
		if *openHTML {
			if err := OpenInBrowser(abs); err != nil {
				fmt.Fprintf(os.Stderr, "自动打开失败（可手动双击该文件）：%v\n", err)
			}
		}
	}

	if !*noSave && *format != "json" {
		fmt.Printf("后续可用：winclean dirs -i %s  /  winclean report --open\n", output)
	}

	if ctx.Err() != nil {
		return ExitInterrupted
	}
	// 有目录因权限被跳过时返回「部分成功」，便于脚本判断是否需要提权重试。
	if res.Stats.SkippedDirs > 0 {
		return ExitPartial
	}
	if errors.Is(err, context.Canceled) {
		return ExitInterrupted
	}
	return ExitOK
}

// startProgressRenderer 启动进度渲染，返回停止函数（停止会阻塞至渲染 goroutine 退出）。
func startProgressRenderer(w *os.File, p *scan.Progress, start time.Time) func() {
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(120 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				clearProgressLine(w)
				return
			case <-ticker.C:
				renderProgress(w, p, start)
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
}

func renderProgress(w *os.File, p *scan.Progress, start time.Time) {
	s := p.Snapshot()
	cur := s.Current
	// 截断过长的路径，避免进度行刷屏折行
	if len(cur) > 70 {
		cur = "…" + cur[len(cur)-69:]
	}
	fmt.Fprintf(w, "\r\x1b[K扫描中 %s 文件 · %s · 跳过 %d · %s   %s",
		report.Count(s.Files), report.Bytes(s.Bytes), s.Skipped,
		report.Duration(time.Since(start)), cur)
}

func clearProgressLine(w *os.File) {
	fmt.Fprint(w, "\r\x1b[K")
}

// isTerminal 报告文件是否为字符设备（即交互式终端）。
// 非终端时自动关闭进度显示，避免污染管道输出。
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
