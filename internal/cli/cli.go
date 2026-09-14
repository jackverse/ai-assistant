// Package cli 实现命令行入口与各子命令。
//
// 本期（M1）刻意只使用标准库 flag 而不引入 cobra：
// 命令树只有 4 个命令，标准库足够；且零外部依赖意味着
// 在无法访问 proxy.golang.org 的网络下依然可以离线构建。
// 待命令树增长到十余个（plan/apply/rules 等）时再评估迁移。
package cli

import (
	"fmt"
	"io"
	"os"

	"winclean/internal/winapi"
)

// 退出码约定（见 docs/design/08-数据模型与CLI.md §7）。
const (
	ExitOK            = 0
	ExitError         = 1
	ExitUsage         = 2
	ExitNoScanFile    = 3
	ExitRulesInvalid  = 4
	ExitPrecheckFail  = 5
	ExitPartial       = 6
	ExitNeedAdmin     = 7
	ExitInterrupted   = 130
)

// Run 是 CLI 主入口，返回进程退出码。
func Run(args []string) int {
	// 中文环境下 cmd.exe 默认代码页是 GBK，必须切到 UTF-8，
	// 否则中文路径会显示为乱码（实测到「图片」变成「ͼƬ」）。
	winapi.SetConsoleUTF8()

	if len(args) == 0 {
		usage(os.Stdout)
		return ExitUsage
	}

	switch args[0] {
	case "version", "-v", "--version":
		return cmdVersion(os.Stdout)
	case "doctor":
		return cmdDoctor(args[1:])
	case "scan":
		return cmdScan(args[1:])
	case "dirs":
		return cmdDirs(args[1:])
	case "report":
		return cmdReport(args[1:])
	case "gui":
		return cmdGUI(args[1:])
	case "help", "-h", "--help":
		usage(os.Stdout)
		return ExitOK
	default:
		fmt.Fprintf(os.Stderr, "未知命令：%s\n\n", args[0])
		usage(os.Stderr)
		return ExitUsage
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `winclean — Windows 磁盘空间治理工具

用法：
  winclean <命令> [参数]

命令：
  scan      扫描磁盘，统计占用并产出 scan.json
  report    从 scan.json 渲染报告（HTML / JSON / 终端表格）
  dirs      从 scan.json 读取并列出目录占用排行
  gui       双击即用的入口：扫描（或复用结果）→ 生成 HTML → 打开浏览器
  doctor    环境自检（权限、磁盘、编码、硬链接去重能力）
  version   显示版本
  help      显示本帮助

想直接看界面：
  winclean gui                  # 复用已有 scan.json，生成并打开 HTML 报告
  winclean gui --reuse=false    # 重新扫描后再打开
  winclean report --open        # 只重新渲染报告并打开

scan 常用参数：
  --disk C,D        指定要扫描的盘（默认系统盘）
  --path DIR        定点扫描某个目录（可重复）
  --deep            深度模式：读取真实磁盘占用并去重硬链接（较慢但精确）
  --max-depth N     上报的目录层级上限（默认 4）
                    注意：这只影响【上报粒度】，遍历始终是全深度的，
                    因为父目录的总量必须包含所有后代，否则总量会偏小而误导判断
  --min-size SIZE   只上报不小于该体积的目录（默认 10MB）
  --exclude DIR     完全跳过该目录（可重复）
  --top-files N     收集占用最大的 N 个文件（默认 30，0 表示不收集）
  --workers N       并发数（默认 CPU*2，上限 32）
  --html FILE       同时生成 HTML 报告
  --open            生成 HTML 后用浏览器打开
  -o FILE           输出 scan.json 的路径（默认 winclean-scan.json）
  --format FORMAT   table（默认）或 json
  --no-save         不写 scan.json
  -q, --quiet       不显示进度

示例：
  winclean gui
  winclean scan --disk C --html report.html --open
  winclean scan --disk C,D --deep -o d:\scan.json
  winclean scan --path "C:\Users"
  winclean dirs --top 30 --min-size 1GB
  winclean dirs --reparse
`)
}
