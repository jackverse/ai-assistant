// Command winclean 是 Windows 磁盘空间治理工具的命令行入口。
//
// 设计文档见 docs/design/。当前处于 M1（扫描骨架）阶段：
// 全部功能只读，不删除、不迁移、不修改任何系统设置。
package main

import (
	"os"

	"winclean/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
