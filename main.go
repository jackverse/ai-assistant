// 助手（Windows 助手）— 磁盘空间治理工具的集成入口。
//
// 同一个 exe 有两种用法，这是刻意设计：
//
//	双击 / 无参数  → 图形界面（GUI 子系统，不弹黑窗口）
//	命令行带参数  → CLI 模式，输出继承当前终端
//
// 这样「点开就用」和「脚本自动化」两种诉求由同一份逻辑满足，
// 不会出现界面与命令行行为不一致的问题。
//
// 设计文档见 docs/design/。当前完成 M1（扫描）；
// 清理与迁移执行（M7）尚未实现，因此本工具目前只读。
package main

import (
	"os"

	"winclean/internal/cli"
)

func main() {
	if len(os.Args) > 1 {
		// api 走 Headless 路径：只构造 App，不开窗口。
		// 这样 AI 可以用一条命令驱动本软件（见 docs/design/22）。
		if os.Args[1] == "api" {
			os.Exit(cmdAPI(os.Args[2:]))
		}
		os.Exit(cli.Run(os.Args[1:]))
	}
	runGUI()
}
