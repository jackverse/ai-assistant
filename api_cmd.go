package main

// `助手.exe api ...` —— 对外能力接口的 CLI 形态（见 docs/design/22）。
//
// 为什么默认用 CLI 而不是 HTTP：
//   · 零常驻——不起监听、不占端口、进程退出即结束，
//     与项目规则「不常驻服务」完全兼容
//   · AI 本来就会执行命令并读 JSON，这是最自然的调用方式
//
// 用法：
//   api list                      列出全部能力
//   api describe <action>         查看某个能力的参数说明
//   api call <action> [--json '{...}'] [--confirm]
//
// 这个入口走 Headless 路径：只构造 App、不开窗口、不初始化界面。

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"winclean/internal/api"
	"winclean/internal/config"
)

func cmdAPI(args []string) int {
	if len(args) == 0 {
		apiUsage()
		return 2
	}

	switch args[0] {
	case "list", "ls":
		return apiList()
	case "describe", "desc":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "用法: api describe <action>")
			return 2
		}
		return apiDescribe(args[1])
	case "call":
		return apiCall(args[1:])
	case "help", "-h", "--help":
		apiUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n\n", args[0])
		apiUsage()
		return 2
	}
}

func apiUsage() {
	fmt.Fprint(os.Stderr, `对外能力接口（供 AI / 脚本调用）

用法：
  助手.exe api list                      列出全部能力
  助手.exe api describe <action>         查看参数说明与示例
  助手.exe api call <action> [参数]       调用能力
  助手.exe api help

call 的参数形式：
  --json '{"disk":"C"}'   以 JSON 传参（推荐，AI 用这个）
  --confirm               危险动作（kind=destructive）必须显式带上
  --pretty                美化输出（默认已是缩进 JSON）

示例：
  助手.exe api call disk.scan_start --json '{"disk":"C"}'
  助手.exe api call disk.scan_status
  助手.exe api call disk.decision
  助手.exe api call chat.analyze_scan
  助手.exe api call deploy.precheck

响应信封：{"ok":true,"data":{...}} 或 {"ok":false,"error":"...","hint":"怎么修"}
`)
}

// newHeadlessApp 构造一个不开窗口的 App（供 CLI 使用）。
func newHeadlessApp() *App {
	cfg, _, err := config.Load()
	if err != nil {
		cfg = config.Default()
	}
	return NewApp(cfg)
}

func newRegistryFor(app *App) *api.Registry {
	reg := api.NewRegistry()
	registerAllActions(app, reg)
	return reg
}

func emitJSON(v any) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "输出失败: %v\n", err)
		return 1
	}
	return 0
}

func apiList() int {
	app := newHeadlessApp()
	reg := newRegistryFor(app)

	infos := reg.List()
	return emitJSON(api.Response{OK: true, Action: "api.list", Data: map[string]any{
		"count":   len(infos),
		"modules": reg.Modules(),
		"actions": infos,
		"usage":   "助手.exe api call <id> --json '{...}'；危险动作需加 --confirm",
	}})
}

func apiDescribe(id string) int {
	app := newHeadlessApp()
	reg := newRegistryFor(app)

	info, ok := reg.Describe(id)
	if !ok {
		return emitJSON(api.Response{
			OK: false, Action: id,
			Error: "没有这个能力: " + id,
			Hint:  "先执行 `助手.exe api list` 查看全部可用能力",
		})
	}
	return emitJSON(api.Response{OK: true, Action: id, Data: info})
}

func apiCall(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: api call <action> [--json '{...}'] [--confirm]")
		return 2
	}
	actionID := args[0]

	fs := flag.NewFlagSet("api call", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	jsonArgs := fs.String("json", "", "以 JSON 传参")
	confirm := fs.Bool("confirm", false, "危险动作确认")
	fs.BoolVar(confirm, "y", false, "危险动作确认（简写）")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	callArgs := map[string]any{}
	if s := strings.TrimSpace(*jsonArgs); s != "" {
		if err := json.Unmarshal([]byte(s), &callArgs); err != nil {
			return emitJSON(api.Response{
				OK: false, Action: actionID,
				Error: "参数不是合法 JSON: " + err.Error(),
				Hint:  `参数要用 JSON 对象，例如 --json '{"disk":"C"}'；注意 Windows 下用单引号包裹避免转义`,
			})
		}
	}

	app := newHeadlessApp()
	reg := newRegistryFor(app)

	resp := reg.Call(context.Background(), actionID, api.CallOptions{
		Args: callArgs, Confirm: *confirm,
	})
	code := emitJSON(resp)
	if !resp.OK {
		return 1
	}
	return code
}
