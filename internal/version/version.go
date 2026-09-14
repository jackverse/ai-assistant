// Package version 保存构建版本信息。
// 通过链接期注入：go build -ldflags "-X winclean/internal/version.Version=..."
package version

import (
	"fmt"
	"runtime"
)

var (
	// Version 由 Makefile 或构建脚本注入，默认 dev。
	Version = "dev"
	// Commit 是 git commit 短哈希。
	Commit = "none"
	// BuildTime 是构建时间（RFC3339）。
	BuildTime = "unknown"
)

// String 返回单行版本描述。
func String() string {
	return fmt.Sprintf("winclean %s (commit %s, built %s, %s/%s, %s)",
		Version, Commit, BuildTime, runtime.GOOS, runtime.GOARCH, runtime.Version())
}
