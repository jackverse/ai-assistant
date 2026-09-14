// Package migrate 实现空间迁移：把 C 盘上的大目录搬到其他盘，
// 在原位置留下目录联接（Junction），软件无感知，可整体回滚。
//
// 机制选择遵循 docs/design/07 §1.1 的决策树：能用官方机制（应用配置、
// 环境变量）就不用 Junction。本包提供两种官方化程度不同的方式：
//   - Junction（通用兜底，无需提权，软件无感知）—— 所有候选都支持；
//   - 环境变量（对声明了 EnvVar 的候选可同时设置）—— 让官方机制也生效。
//
// 明确不做（07 §8）：迁移已安装程序、系统目录、OneDrive 接管的目录。
package migrate

import (
	"os"
	"strings"

	"winclean/internal/sys"
)

// Risk 是迁移风险等级（docs/design/07 §6）。
type Risk string

const (
	RiskLow    Risk = "low"
	RiskMedium Risk = "medium"
)

// Cand 是一个可迁移候选。
type Cand struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"` // 已展开的真实路径；存在才出现在列表里
	Size int64  `json:"size"` // 逻辑大小（测量填充）

	// EnvVar 非空时可在迁移时一并设置用户级环境变量指向新位置。
	// 官方机制优先于 Junction——设置后软件直接使用新路径。
	EnvVar string `json:"env_var,omitempty"`
	EnvNow string `json:"env_now,omitempty"`

	Risk   Risk    `json:"risk"`
	RiskWhy string `json:"risk_why"`
	Note   string  `json:"note,omitempty"`
}

// Catalog 返回当前机器上实际存在的迁移候选。
//
// 路径全部出自内置清单（docs/design/07 §10 的开发工具缓存部分）。
// 每一项都是「缓存或可重建数据」——误判的代价只是重新下载，
// 绝不包含用户创作内容（文档、聊天记录在候选集里根本不存在）。
func Catalog() []Cand {
	profile := envPath("USERPROFILE")
	local := envPath("LOCALAPPDATA")
	appdata := envPath("APPDATA")

	type def struct {
		id, name, base string
		sub            string
		envVar         string
		risk           Risk
		riskWhy        string
		note           string
	}
	defs := []def{
		{"npm_cache", "npm 包缓存", local, `npm-cache`, "NPM_CONFIG_CACHE", RiskLow, "", ""},
		{"yarn_cache", "Yarn 包缓存", local, `Yarn\Cache`, "YARN_CACHE_FOLDER", RiskLow, "", ""},
		{"pip_cache", "pip 缓存", local, `pip\Cache`, "PIP_CACHE_DIR", RiskLow, "", ""},
		{"pnpm_store", "pnpm 包仓库", local, `pnpm\store`, "", RiskLow, "", ""},
		{"maven_repo", "Maven 本地仓库", profile, `.m2\repository`, "", RiskLow,
			"", "官方做法是改 settings.xml 的 localRepository；Junction 效果等价"},
		{"gradle_caches", "Gradle 缓存", profile, `.gradle\caches`, "", RiskLow, "", ""},
		{"go_build", "Go 构建缓存", local, `go-build`, "GOCACHE", RiskLow, "", ""},
		{"go_mod", "Go 模块缓存", profile, `go\pkg\mod`, "GOMODCACHE", RiskLow, "", ""},
		{"nuget_pkgs", "NuGet 包缓存", profile, `.nuget\packages`, "NUGET_PACKAGES", RiskLow, "", ""},
		{"user_cache", "用户缓存目录（.cache）", profile, `.cache`, "", RiskLow,
			"", "内部各工具的缓存变量不统一，用 Junction 整体兜底"},
		{"nvm", "nvm（Node 版本管理）", appdata, `nvm`, "NVM_HOME", RiskLow, "", ""},
		{"android_sdk", "Android SDK", local, `Android\Sdk`, "ANDROID_SDK_ROOT", RiskMedium,
			"迁移后需同步更新 Android Studio 的 SDK 路径与各项目 local.properties 里的 sdk.dir，否则现有项目构建会失败",
			"建议迁移后逐个项目检查 local.properties"},
		{"user_temp", "用户临时目录（Temp）", local, `Temp`, "", RiskMedium,
			"部分安装程序对跨盘临时目录有假设；同盘 rename 退化为复制粘贴，个别场景变慢",
			"收益小时不建议迁移，直接清理更好"},
	}

	var out []Cand
	for _, d := range defs {
		if d.base == "" {
			continue
		}
		p := d.base + `\` + d.sub
		if !exists(p) {
			continue
		}
		// 已经是 Junction 的（此前迁移过）：不重复出现在候选里
		if is, _, err := sys.IsReparsePoint(p); err == nil && is {
			continue
		}
		c := Cand{
			ID: d.id, Name: d.name, Path: p,
			EnvVar: d.envVar, Risk: d.risk, RiskWhy: d.riskWhy, Note: d.note,
		}
		if d.envVar != "" {
			c.EnvNow = os.Getenv(d.envVar)
		}
		out = append(out, c)
	}
	return out
}

func envPath(name string) string {
	v := os.Getenv(name)
	if v == "" {
		return ""
	}
	abs, err := sys.CleanAbsolute(v)
	if err != nil {
		return ""
	}
	return strings.TrimRight(abs, `\`)
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
