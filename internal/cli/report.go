package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"time"

	"winclean/internal/report"
	"winclean/internal/scan"
)

// cmdReport 从已有的 scan.json 渲染报告。
//
// 与 scan 分离是有意的：扫描是 O(文件数) 的重操作，报告渲染是毫秒级。
// 分离后可以在一份扫描结果上反复调整展示方式，不必重扫。
func cmdReport(args []string) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { usage(os.Stderr) }

	input := "winclean-scan.json"
	fs.StringVar(&input, "i", input, "scan.json 路径")
	fs.StringVar(&input, "input", input, "scan.json 路径")

	var (
		format  = fs.String("format", "html", "输出格式 html|json|table|md")
		output  = fs.String("o", "winclean-report.html", "输出文件（html/md 用）")
		maxDirs = fs.Int("top", 300, "报告中最多列出多少个目录")
		open    = fs.Bool("open", false, "生成后用默认浏览器打开")
	)
	fs.StringVar(output, "output", *output, "输出文件")

	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}

	res, err := report.ReadJSON(input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return ExitNoScanFile
	}

	switch *format {
	case "html":
		if err := report.WriteHTML(*output, res, *maxDirs); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return ExitError
		}
		abs, _ := filepath.Abs(*output)
		fmt.Printf("报告已生成：%s\n", abs)
		if *open {
			if err := OpenInBrowser(abs); err != nil {
				fmt.Fprintf(os.Stderr, "自动打开失败（可手动双击该文件）：%v\n", err)
				return ExitOK
			}
		}
	case "table":
		report.ScanTable(os.Stdout, res, 30, 15)
	case "json":
		data, err := report.MarshalJSONView(res, *maxDirs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return ExitError
		}
		os.Stdout.Write(data)
	default:
		fmt.Fprintf(os.Stderr, "暂不支持的格式：%s（可用 html / json / table）\n", *format)
		return ExitUsage
	}

	return ExitOK
}

// OpenInBrowser 用系统默认程序打开本地文件。
//
// 用 rundll32 的 FileProtocolHandler 而不是 `cmd /c start`：
// 后者会把路径里的 & 等字符当作 shell 元字符，遇到含特殊字符的路径会失败。
func OpenInBrowser(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("文件不存在: %w", err)
	}
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", path)
	if err := cmd.Start(); err != nil {
		return err
	}
	// 不等待：浏览器是长驻进程，等它会挂住当前进程
	go func() { _ = cmd.Wait() }()
	return nil
}

// cmdGUI 是「双击即用」的入口：扫描（或复用结果）→ 生成 HTML → 打开浏览器。
//
// 存在的理由：这是个命令行工具，双击 exe 只会闪一下黑窗口。
// 给不熟悉命令行的使用者提供一个可以双击的入口。
func cmdGUI(args []string) int {
	fs := flag.NewFlagSet("gui", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { usage(os.Stderr) }

	var (
		disk    = fs.String("disk", "", "要扫描的盘（默认系统盘）")
		deep    = fs.Bool("deep", false, "深度模式（慢，但体积精确）")
		reuse   = fs.Bool("reuse", true, "若已存在 scan.json 则直接复用，不重新扫描")
		html    = fs.String("html", "winclean-report.html", "HTML 报告输出路径")
		jsonOut = fs.String("json", "winclean-scan.json", "scan.json 输出路径")
	)

	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}

	res, err := report.ReadJSON(*jsonOut)
	if err != nil || !*reuse {
		// 需要重新扫描
		roots := []string{}
		if *disk != "" {
			roots = append(roots, *disk+`:\\`)
		}
		fmt.Fprintln(os.Stderr, "正在扫描，请稍候……")
		ctx, stop := signalContext()
		defer stop()

		opts := scan.Options{Roots: roots, Deep: *deep, Progress: scan.NewProgress()}
		renderStop := startProgressRenderer(os.Stderr, opts.Progress, time.Now())
		res, err = scan.Run(ctx, opts)
		renderStop()
		if err != nil {
			fmt.Fprintf(os.Stderr, "扫描失败：%v\n", err)
			return ExitError
		}
		if err := report.WriteJSON(*jsonOut, res); err != nil {
			fmt.Fprintf(os.Stderr, "写入 %s 失败：%v\n", *jsonOut, err)
		}
	} else {
		fmt.Printf("复用已有扫描结果 %s（扫描于 %s）\n", *jsonOut, res.ScannedAt.Format("2006-01-02 15:04:05"))
		fmt.Println("如需重新扫描，加 --reuse=false")
	}

	if err := report.WriteHTML(*html, res, 300); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return ExitError
	}
	abs, _ := filepath.Abs(*html)
	fmt.Printf("报告已生成：%s\n", abs)

	if err := OpenInBrowser(abs); err != nil {
		fmt.Fprintf(os.Stderr, "自动打开失败，请手动双击 %s（%v）\n", abs, err)
	}
	return ExitOK
}

// signalContext 返回一个在 Ctrl+C 时取消的 context。
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}
