package main

import (
	"embed"
	"fmt"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"winclean/internal/config"
)

// 前端资源随二进制内嵌，因此最终产物仍是单个 exe。
// frontend/dist 是手工维护的纯 HTML/CSS/JS，没有构建步骤，
// 也就不需要 npm——这延续了本工具「零外部依赖、可离线构建」的原则。
//
//go:embed all:frontend/dist
var assets embed.FS

// runGUI 启动图形界面。
//
// 轻启动原则：这里只做「读配置 + 建模块注册表」两件事，
// 任何模块的后端（包括磁盘枚举、环境自检）都不初始化——
// 它们发生在用户真正进入对应模块页面时。
func runGUI() {
	cfg, warns, err := config.Load()
	if err != nil {
		cfg = config.Default()
	}

	app := NewApp(cfg)
	app.SetBootWarnings(warns)

	err = wails.Run(&options.App{
		Title:     "Windows 助手",
		Width:     1160,
		Height:    800,
		MinWidth:  900,
		MinHeight: 620,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 15, G: 17, B: 21, A: 255},
		OnStartup:        app.startup,
		Bind:             []interface{}{app},
		Windows: &windows.Options{
			Theme:                windows.Dark,
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
		},
	})
	if err != nil {
		// 界面起不来时至少要把原因说清楚（例如 WebView2 缺失）
		fmt.Fprintf(os.Stderr, "界面启动失败: %v\n", err)
		fmt.Fprintln(os.Stderr, "提示: 本界面依赖 WebView2 运行时（Windows 11 通常已内置）。")
		fmt.Fprintln(os.Stderr, "      你仍可以用命令行方式使用全部功能，例如: 助手.exe scan --disk C")
		os.Exit(1)
	}
}
