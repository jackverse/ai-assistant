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
	"path/filepath"
	"strings"

	"winclean/internal/sys"
)

// Risk 是迁移风险等级（docs/design/07 §6）。
type Risk string

const (
	RiskLow    Risk = "low"
	RiskMedium Risk = "medium"
)

// Role 描述一个路径在应用程序里的用途（用户视角的「地址类型」）。
type Role string

const (
	// RoleWork 工作地址：程序的安装/运行时所在（SDK、版本管理器）。
	RoleWork Role = "work"
	// RoleData 数据存储地址：聊天记录、文档、容器镜像等用户数据。
	RoleData Role = "data"
	// RoleCache 缓存地址：可重建的缓存。
	RoleCache Role = "cache"
)

// RoleName 返回角色给用户看的名称。
func RoleName(r Role) string {
	switch r {
	case RoleWork:
		return "工作地址"
	case RoleData:
		return "数据存储地址"
	case RoleCache:
		return "缓存地址"
	}
	return "其他"
}

// Cand 是一个可迁移候选。
type Cand struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"` // 已展开的真实路径；存在才出现在列表里
	Size int64  `json:"size"` // 逻辑大小（测量填充）

	// App / AppIcon / Role 用于「按应用」分组展示。
	// App 为空表示这是独立的缓存项（不归属某个应用）。
	App     string `json:"app,omitempty"`
	AppIcon string `json:"app_icon,omitempty"`
	Role    Role   `json:"role,omitempty"`

	// EnvVar 非空时可在迁移时一并设置用户级环境变量指向新位置。
	// 官方机制优先于 Junction——设置后软件直接使用新路径。
	EnvVar string `json:"env_var,omitempty"`
	EnvNow string `json:"env_now,omitempty"`

	Risk    Risk    `json:"risk"`
	RiskWhy string  `json:"risk_why"`
	Note    string  `json:"note,omitempty"`
}

// appDef 描述一个已知应用及其可迁移地址。
type appDef struct {
	id    string
	name  string
	icon  string
	paths []appPath
}

type appPath struct {
	role   Role
	base   string // 环境变量名（根目录）
	sub    string // 相对子路径
	risk   Risk
	why    string
	note   string
	envVar string
}

// AppCatalog 返回已知应用的全部可迁移地址（存在才列出）。
//
// 这里的定位是「应用地址总表」：工作地址 / 数据存储地址 / 缓存地址
// 三类角色，让用户按应用视角理解自己要搬的是什么。
// 迁移动作复用统一的 Junction 管线（Run/Undo/PurgeBackup）。
func AppCatalog() []Cand {
	local := envPath("LOCALAPPDATA")
	appdata := envPath("APPDATA")
	docs := documentsDir()

	quitNote := "迁移前请先退出该应用，否则切换步骤会因文件被占用而失败"

	defs := []appDef{
		{id: "wechat", name: "微信", icon: "💬", paths: []appPath{
			{role: RoleData, base: docs, sub: `WeChat Files`, risk: RiskMedium,
				why: "聊天记录与接收的文件属于重要用户数据；迁移前请确认已退出微信并做好备份",
				note: "官方途径是微信内「设置 → 文件管理」改存储位置；Junction 为免提权兜底。" + quitNote},
			{role: RoleData, base: docs, sub: `xwechat_files`, risk: RiskMedium,
				why: "新版微信的数据目录，同上",
				note: quitNote},
		}},
		{id: "qq", name: "QQ", icon: "🐧", paths: []appPath{
			{role: RoleData, base: docs, sub: `Tencent Files`, risk: RiskMedium,
				why: "聊天记录与接收的文件属于重要用户数据",
				note: "建议先退出 QQ。" + quitNote},
		}},
		{id: "docker", name: "Docker Desktop", icon: "🐳", paths: []appPath{
			{role: RoleData, base: local, sub: `Docker`, risk: RiskMedium,
				why: "镜像与容器数据（含虚拟磁盘），通常是 C 盘最大的单项之一",
				note: "需退出 Docker Desktop 并执行 wsl --shutdown；官方途径是设置里的 Disk image location，Junction 兜底"},
		}},
		{id: "chrome", name: "Google Chrome", icon: "🌐", paths: []appPath{
			{role: RoleCache, base: local, sub: `Google\Chrome\User Data\Default\Cache`, risk: RiskLow, note: quitNote},
			{role: RoleCache, base: local, sub: `Google\Chrome\User Data\Default\Code Cache`, risk: RiskLow, note: quitNote},
		}},
		{id: "edge", name: "Microsoft Edge", icon: "🌐", paths: []appPath{
			{role: RoleCache, base: local, sub: `Microsoft\Edge\User Data\Default\Cache`, risk: RiskLow, note: quitNote},
			{role: RoleCache, base: local, sub: `Microsoft\Edge\User Data\Default\Code Cache`, risk: RiskLow, note: quitNote},
		}},
		{id: "vscode", name: "VS Code", icon: "📝", paths: []appPath{
			{role: RoleData, base: appdata, sub: `Code`, risk: RiskMedium,
				why: "含用户设置（settings.json）、扩展与工作区数据",
				note: quitNote},
		}},
		{id: "netease_music", name: "网易云音乐", icon: "🎵", paths: []appPath{
			{role: RoleCache, base: local, sub: `Netease\CloudMusic\Cache`, risk: RiskLow, note: quitNote},
		}},
		{id: "android_sdk", name: "Android SDK", icon: "🤖", paths: []appPath{
			{role: RoleWork, base: local, sub: `Android\Sdk`, risk: RiskMedium, envVar: "ANDROID_SDK_ROOT",
				why: "迁移后需同步更新 Android Studio 的 SDK 路径与各项目 local.properties 里的 sdk.dir，否则现有项目构建会失败",
				note: "建议同时设置环境变量 ANDROID_SDK_ROOT"},
		}},
	}

	var out []Cand
	for _, d := range defs {
		for _, p := range d.paths {
			if p.base == "" {
				continue
			}
			path := filepath.Clean(p.base + `\` + p.sub)
			if !exists(path) {
				continue
			}
			if is, _, err := sys.IsReparsePoint(path); err == nil && is {
				continue // 已迁移过
			}
			out = append(out, Cand{
				ID:   d.id + ":" + p.sub, // 含子路径，保证唯一
				Name: RoleName(p.role),
				Path: path,
				App:  d.name, AppIcon: d.icon, Role: p.role,
				EnvVar: p.envVar, Risk: p.risk, RiskWhy: p.why, Note: p.note,
			})
		}
	}
	return out
}

// documentsDir 返回「文档」Known Folder 的实际位置（可能被重定向）。
//
// 微信/QQ 的数据目录跟着文档目录走，这里必须尽量取准。
// USERPROFILE\Documents 覆盖绝大多数机器；再探「文档」（少数中文系统
// 改名场景）。两者都不在则返回空（相关应用条目自动省略）。
func documentsDir() string {
	profile := envPath("USERPROFILE")
	if profile == "" {
		return ""
	}
	for _, cand := range []string{`Documents`, `文档`} {
		p := profile + `\` + cand
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return p
		}
	}
	return ""
}

// Catalog 返回全部迁移候选 = 已知应用地址 + 开发工具缓存。
func Catalog() []Cand {
	out := AppCatalog()
	out = append(out, devCacheCatalog()...)
	return out
}

// devCacheCatalog 是按目录视角的开发缓存清单。
func devCacheCatalog() []Cand {
	profile := envPath("USERPROFILE")
	local := envPath("LOCALAPPDATA")
	appdata := envPath("APPDATA")

	type def struct {
		id, name, base string
		sub            string
		envVar         string
		role           Role
		risk           Risk
		riskWhy        string
		note           string
	}
	defs := []def{
		{"npm_cache", "npm 包缓存", local, `npm-cache`, "NPM_CONFIG_CACHE", RoleCache, RiskLow, "", ""},
		{"yarn_cache", "Yarn 包缓存", local, `Yarn\Cache`, "YARN_CACHE_FOLDER", RoleCache, RiskLow, "", ""},
		{"pip_cache", "pip 缓存", local, `pip\Cache`, "PIP_CACHE_DIR", RoleCache, RiskLow, "", ""},
		{"pnpm_store", "pnpm 包仓库", local, `pnpm\store`, "", RoleCache, RiskLow, "", ""},
		{"maven_repo", "Maven 本地仓库", profile, `.m2\repository`, "", RoleCache, RiskLow,
			"", "官方做法是改 settings.xml 的 localRepository；Junction 效果等价"},
		{"gradle_caches", "Gradle 缓存", profile, `.gradle\caches`, "", RoleCache, RiskLow, "", ""},
		{"go_build", "Go 构建缓存", local, `go-build`, "GOCACHE", RoleCache, RiskLow, "", ""},
		{"go_mod", "Go 模块缓存", profile, `go\pkg\mod`, "GOMODCACHE", RoleCache, RiskLow, "", ""},
		{"nuget_pkgs", "NuGet 包缓存", profile, `.nuget\packages`, "NUGET_PACKAGES", RoleCache, RiskLow, "", ""},
		{"user_cache", "用户缓存目录（.cache）", profile, `.cache`, "", RoleCache, RiskLow,
			"", "内部各工具的缓存变量不统一，用 Junction 整体兜底"},
		{"nvm", "nvm（Node 版本管理）", appdata, `nvm`, "NVM_HOME", RoleWork, RiskLow, "", ""},
		{"android_sdk", "Android SDK", local, `Android\Sdk`, "ANDROID_SDK_ROOT", RoleWork, RiskMedium,
			"迁移后需同步更新 Android Studio 的 SDK 路径与各项目 local.properties 里的 sdk.dir，否则现有项目构建会失败",
			"建议迁移后逐个项目检查 local.properties"},
		{"user_temp", "用户临时目录（Temp）", local, `Temp`, "", RoleWork, RiskMedium,
			"部分安装程序对跨盘临时目录有假设；同盘 rename 退化为复制粘贴，个别场景变慢",
			"收益小时不建议迁移，直接清理更好"},
	}

	var out []Cand
	for _, d := range defs {
		if d.base == "" {
			continue
		}
		p := filepath.Clean(d.base + `\` + d.sub)
		if !exists(p) {
			continue
		}
		// 已经是 Junction 的（此前迁移过）：不重复出现在候选里
		if is, _, err := sys.IsReparsePoint(p); err == nil && is {
			continue
		}
		c := Cand{
			ID: d.id, Name: d.name, Path: p,
			App: "开发工具缓存", AppIcon: "🧰", Role: d.role,
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
