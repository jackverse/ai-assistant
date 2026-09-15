package report

// 安全判定：决定一个路径能不能进「绿色区（可以删除）」。
//
// 这里刻意用【白名单】而不是关键词黑名单。原因：关键词匹配看起来省事，
// 但会误伤——复查时发现两处真实风险：
//   · `\temp` 会匹配 D:\work\temp、D:\projects\temp 这类用户自建目录
//   · `-updater` 会匹配代码库里叫 xxx-updater 的项目目录
// 对一个开发者来说，这两种误判都会导致删掉自己的工作成果。
//
// 因此绿色区的准入条件是：路径必须落在【已知的可丢弃位置】之内，
// 并且必须通过项目目录检查。

import (
	"os"
	"path/filepath"
	"strings"
)

// SafeVerdict 是安全判定的结果。
type SafeVerdict struct {
	Safe   bool
	Reason string // 进绿色区的原因（人话）
	Block  string // 被拦下的原因（不为空则绝不能进绿色区）
}

// safeCleanVerdict 判定路径能否进绿色区。
//
// codeRoots 是用户配置的代码根目录，其下任何东西都绝不进绿色区。
func safeCleanVerdict(path string, codeRoots []string) SafeVerdict {
	p := strings.ToLower(filepath.Clean(path))

	// ── 硬保护 1：代码根目录之下，一律不删 ──
	for _, root := range codeRoots {
		root = strings.ToLower(filepath.Clean(root))
		if root == "" {
			continue
		}
		if p == root || strings.HasPrefix(p, root+`\`) {
			return SafeVerdict{Block: "位于你的代码目录内，绝不自动删除"}
		}
	}

	// ── 硬保护 2：看起来是项目目录（含版本控制或构建文件）──
	if looksLikeProject(path) {
		return SafeVerdict{Block: "含项目文件（.git/pom.xml/package.json 等），属于你的工作成果"}
	}

	// ── 硬保护 3：盘根与用户数据目录 ──
	if len(p) == 3 && p[1] == ':' { // "d:\"
		return SafeVerdict{Block: "盘根目录"}
	}
	for _, kw := range []string{
		`\documents`, `\desktop`, `\pictures`, `\videos`, `\music`, `onedrive`,
		`\tencent`, `\wechat`, `\dingtalk`, `\larkshell`,
	} {
		if strings.Contains(p, kw) {
			return SafeVerdict{Block: "属于用户数据"}
		}
	}

	// ── 白名单：只有落在这些位置内才算可安全清理 ──
	local := strings.ToLower(os.Getenv("LOCALAPPDATA"))
	programData := strings.ToLower(os.Getenv("ProgramData"))
	winDir := strings.ToLower(os.Getenv("SystemRoot"))
	if winDir == "" {
		winDir = `c:\windows`
	}

	type rule struct{ prefix, reason string }
	var rules []rule
	if local != "" {
		rules = append(rules,
			rule{local + `\temp`, "用户临时文件，程序退出后通常不再需要"},
			rule{local + `\crashdumps`, "应用崩溃转储文件"},
			rule{local + `\microsoft\windows\wer`, "Windows 错误报告"},
		)
	}
	rules = append(rules,
		rule{winDir + `\temp`, "系统临时文件"},
		rule{winDir + `\softwaredistribution\download`, "Windows 更新下载缓存（已安装的更新不受影响）"},
		rule{winDir + `\logs\cbs`, "组件安装日志"},
		rule{winDir + `\logs\dism`, "DISM 日志"},
		rule{winDir + `\livekernelreports`, "内核诊断报告"},
		rule{winDir + `\minidump`, "小型崩溃转储"},
	)
	if programData != "" {
		rules = append(rules, rule{programData + `\microsoft\windows\wer`, "Windows 错误报告归档"})
	}

	for _, r := range rules {
		if p == r.prefix || strings.HasPrefix(p, r.prefix+`\`) {
			return SafeVerdict{Safe: true, Reason: r.reason}
		}
	}

	// ── 次级白名单：AppData\Local 下一层的「更新器残留 / 安装器残留」──
	// 要求：必须在 LocalAppData 下、必须是已知后缀、且已通过项目检查
	if local != "" && strings.HasPrefix(p, local+`\`) {
		rel := strings.TrimPrefix(p, local+`\`)
		// 只允许 LocalAppData 的一级子目录，避免深层误伤
		if !strings.Contains(rel, `\`) {
			lower := strings.ToLower(rel)
			switch {
			case strings.HasSuffix(lower, `-updater`), strings.HasSuffix(lower, `Updater`):
				return SafeVerdict{Safe: true, Reason: "应用自动更新的残留包，下次更新会重新下载"}
			case strings.Contains(lower, "installer") && !strings.Contains(lower, "program"):
				return SafeVerdict{Safe: true, Reason: "安装包残留，软件已安装后可安全删除"}
			}
		}
	}

	return SafeVerdict{}
}

// looksLikeProject 判断目录是否像「用户的工程目录」。
//
// 只检查一层（不递归）：项目根的正特征文件都在根目录。
// 命中任一即认为不可自动删除——宁可漏删也不能误删工作成果。
func looksLikeProject(dir string) bool {
	markers := []string{
		".git", ".svn", ".hg",
		"pom.xml", "build.gradle", "build.gradle.kts", "settings.gradle",
		"package.json", "go.mod", "Cargo.toml", "requirements.txt",
		"pyproject.toml", "Gemfile", "composer.json",
		".sln", ".csproj", ".gitignore", ".idea", ".vscode",
		"README.md", "AGENTS.md", "CLAUDE.md",
	}
	for _, m := range markers {
		if _, err := os.Stat(filepath.Join(dir, m)); err == nil {
			return true
		}
	}
	return false
}
