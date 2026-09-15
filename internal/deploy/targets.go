package deploy

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// TargetPath 是一个「值得量的路径」及其元信息。
type TargetPath struct {
	Path     string `json:"path"`
	Label    string `json:"label"`    // 人可读的名称（如 "QQ" / "Yarn 缓存"）
	Category string `json:"category"` // installed / cache / data / system
	Source   string `json:"source"`   // registry / pattern / env
}

// DiscoverTargets 从注册表与已知模式发现值得测量的路径。
//
// 这一步【不碰文件系统】——纯注册表读取与模式匹配，毫秒级完成。
// 返回的路径由调用方决定是否测量（跑一次 bounded scan 得到大小）。
func DiscoverTargets(extraRoots []string) ([]TargetPath, error) {
	var out []TargetPath
	seen := map[string]bool{}

	add := func(tp TargetPath) {
		p := strings.ToLower(filepath.Clean(tp.Path))
		if p == "" || seen[p] {
			return
		}
		if !dirExists(p) {
			return
		}
		seen[p] = true
		out = append(out, tp)
	}

	// ── 1. 注册表 Uninstall 键：已安装软件的安装位置 ──
	for _, keyPath := range []string{
		`SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`,
		`SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`,
	} {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, keyPath, registry.ENUMERATE_SUB_KEYS)
		if err != nil {
			continue
		}
		names, err := k.ReadSubKeyNames(-1)
		k.Close()
		if err != nil {
			continue
		}
		for _, name := range names {
			sk, err := registry.OpenKey(registry.LOCAL_MACHINE, keyPath+`\`+name, registry.QUERY_VALUE)
			if err != nil {
				continue
			}
			displayName, _, _ := sk.GetStringValue("DisplayName")
			installLoc, _, _ := sk.GetStringValue("InstallLocation")
			sk.Close()

			displayName = strings.TrimSpace(displayName)
			installLoc = strings.TrimSpace(installLoc)
			if displayName == "" || installLoc == "" || !dirExists(installLoc) {
				continue
			}
			add(TargetPath{
				Path: installLoc, Label: displayName,
				Category: "installed", Source: "registry",
			})
		}
	}

	// ── 2. 用户级 Uninstall 键（per-user 安装） ──
	hkcu, err := registry.OpenKey(registry.CURRENT_USER,
		`SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`, registry.ENUMERATE_SUB_KEYS)
	if err == nil {
		names, err := hkcu.ReadSubKeyNames(-1)
		hkcu.Close()
		if err == nil {
			for _, name := range names {
				sk, err := registry.OpenKey(registry.CURRENT_USER,
					`SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\`+name, registry.QUERY_VALUE)
				if err != nil {
					continue
				}
				displayName, _, _ := sk.GetStringValue("DisplayName")
				installLoc, _, _ := sk.GetStringValue("InstallLocation")
				sk.Close()
				if displayName == "" || installLoc == "" || !dirExists(installLoc) {
					continue
				}
				add(TargetPath{
					Path: installLoc, Label: displayName,
					Category: "installed", Source: "registry-hkcu",
				})
			}
		}
	}

	// ── 3. 已知缓存与数据路径模式 ──
	home, _ := os.UserHomeDir()
	appLocal := filepath.Join(home, "AppData", "Local")
	appRoaming := filepath.Join(home, "AppData", "Roaming")

	patterns := []TargetPath{
		{Label: "Yarn 包缓存", Category: "cache", Path: filepath.Join(appLocal, "Yarn", "Cache")},
		{Label: "npm 包缓存", Category: "cache", Path: filepath.Join(appLocal, "npm-cache")},
		{Label: "npm 包缓存（旧位置）", Category: "cache", Path: filepath.Join(appRoaming, "npm-cache")},
		{Label: "pip 缓存", Category: "cache", Path: filepath.Join(appLocal, "pip", "Cache")},
		{Label: "Playwright 浏览器", Category: "cache", Path: filepath.Join(appLocal, "ms-playwright")},
		{Label: "Cypress", Category: "cache", Path: filepath.Join(appLocal, "Cypress")},
		{Label: "Puppeteer", Category: "cache", Path: filepath.Join(home, ".cache", "puppeteer")},
		{Label: "Go 构建缓存", Category: "cache", Path: filepath.Join(appLocal, "go-build")},
		{Label: "Maven 本地仓库", Category: "cache", Path: filepath.Join(home, ".m2", "repository")},
		{Label: "Gradle 缓存", Category: "cache", Path: filepath.Join(home, ".gradle")},
		{Label: "Go 模块缓存", Category: "cache", Path: filepath.Join(home, "go", "pkg", "mod")},
		{Label: "用户临时文件", Category: "cache", Path: filepath.Join(appLocal, "Temp")},
		{Label: "Windows 更新缓存", Category: "system", Path: `C:\Windows\SoftwareDistribution\Download`},
		{Label: "nvm 多版本 Node", Category: "cache", Path: filepath.Join(appRoaming, "nvm")},
		{Label: "Docker 数据盘", Category: "data", Path: filepath.Join(appLocal, "Docker")},
		{Label: "Android SDK", Category: "data", Path: filepath.Join(appLocal, "Android")},
		{Label: "腾讯系数据", Category: "data", Path: filepath.Join(appRoaming, "Tencent")},
		{Label: "JetBrains 缓存", Category: "cache", Path: filepath.Join(appLocal, "JetBrains")},
		{Label: "VS Code 数据", Category: "data", Path: filepath.Join(appRoaming, "Code")},
		{Label: "Cursor 数据", Category: "data", Path: filepath.Join(appRoaming, "Cursor")},
		{Label: "WPS Office", Category: "data", Path: filepath.Join(appRoaming, "kingsoft")},
		{Label: "钉钉", Category: "data", Path: filepath.Join(appRoaming, "DingTalk")},
		{Label: "飞书", Category: "data", Path: filepath.Join(appRoaming, "LarkShell")},
		{Label: "Puppeteer 缓存(.cache)", Category: "cache", Path: filepath.Join(home, ".cache", "puppeteer")},
	}
	for _, pt := range patterns {
		pt.Source = "known-pattern"
		add(pt)
	}

	// ── 4. 代码根目录下的一级子目录（如果配了的话） ──
	for _, root := range extraRoots {
		if root == "" || !dirExists(root) {
			continue
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				p := filepath.Join(root, e.Name())
				add(TargetPath{Path: p, Label: e.Name(), Category: "code", Source: "code-root"})
			}
		}
	}

	return out, nil
}

// fmtTargetPath 格式化路径展示。
func fmtTargetPath(p string) string {
	return strings.ReplaceAll(p, `\\?\`, "")
}
