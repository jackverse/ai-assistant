package migrate

import "golang.org/x/sys/windows/registry"

// 用户级环境变量读写（HKCU\Environment）。
//
// winapi 包有「不依赖 x/sys」的约束（保证离线可构建）；
// 本包没有该约束，且 x/sys 已在依赖图内（go.sum 在案），直接使用。

func registryOpen() (registry.Key, error) {
	return registry.OpenKey(registry.CURRENT_USER, `Environment`,
		registry.QUERY_VALUE|registry.SET_VALUE)
}

func registryNotFound() error { return registry.ErrNotExist }
