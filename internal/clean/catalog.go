// Package clean 实现垃圾清理：候选目录发现、可清理性分级、体积测量
// 与安全删除。
//
// 等级模型与判定规则来自 docs/design/06-可清理性分级.md，是硬约束：
//   - 只有 L2（SAFE）默认勾选，且必须百分之百安全；
//   - L1（CONFIRM）默认不勾选，由用户逐项开启；
//   - L0（NEVER）根本不出现在候选列表里——不提供「强制删除」入口；
//   - 未匹配规则的路径绝不给 L2（「不知道 → 不删」）。
//
// 因此本包的候选集是【内置固定目录白名单】（对应 06 §5 的系统路径基线），
// 不做全盘启发式扫描。这样每一项都有明确、可辩护的理由。
package clean

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"winclean/internal/sys"
)

// Level 是可清理性等级（对外用字符串，便于 JSON 序列化给前端）。
type Level string

const (
	// LevelConfirm 需确认：有风险、影响体验或属于用户数据（默认不勾选）。
	LevelConfirm Level = "L1"
	// LevelSafe 可安全清理：可重建、无副作用或副作用极小（默认勾选）。
	LevelSafe Level = "L2"
)

// Kind 决定测量与删除的方式。
type Kind string

const (
	// KindDir 清空整个目录的内容（保留目录本身），递归。
	KindDir Kind = "dir"
	// KindFiles 只删目录内匹配模式的文件（不递归）。
	KindFiles Kind = "files"
	// KindFile 删除 Paths 里列出的单个精确文件。
	KindFile Kind = "file"
	// KindDownloads 下载目录：按文件列出让用户逐个勾选。
	KindDownloads Kind = "downloads"
)

// Candidate 是一个可清理候选。
type Candidate struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Level Level  `json:"level"`
	Kind  Kind   `json:"kind"`

	// Paths 是实际参与测量的目录（UWP 缓存这类会展开成多个子目录）。
	Paths []string `json:"paths"`

	// MinAge 是文件删除的最小年龄（距最后修改时间）。
	// 临时文件 24h（必须，否则可能删掉正在写入的文件）；
	// 日志/转储 7d（诊断价值窗口）。
	MinAge time.Duration `json:"min_age"`

	// Patterns 只对 KindFiles 生效（如 thumbcache_*.db）。
	Patterns []string `json:"patterns,omitempty"`

	AdminRequired bool   `json:"admin_required"`
	Reason        string `json:"reason"`
	Consequence   string `json:"consequence"`
	Note          string `json:"note,omitempty"`

	// AllowedRoot 记录该候选归属的允许根（护栏校验用，不输出给前端）。
	allowedRoot string
}

// Catalog 返回当前机器上实际存在的清理候选（已展开环境变量与 UWP 子目录）。
//
// 不存在的候选直接省略——列表里出现一个 0 字节的「候选」只会制造噪音。
func Catalog() []Candidate {
	profile := envPath("USERPROFILE")
	local := envPath("LOCALAPPDATA")
	programData := envPath("ProgramData")
	winRoot := windowsRoot()

	var out []Candidate
	add := func(c Candidate) {
		// 过滤不存在的路径；只要有一个子路径存在就保留
		anyExists := false
		for _, p := range c.Paths {
			if p != "" && dirOrFileExists(p) {
				anyExists = true
				break
			}
		}
		if anyExists && c.allowedRoot != "" {
			out = append(out, c)
		}
	}

	if local != "" {
		add(Candidate{ID: "temp_user", Title: "用户临时文件", Level: LevelSafe, Kind: KindDir,
			Paths: []string{local + `\Temp`}, MinAge: 24 * time.Hour,
			Reason: "用户与安装程序留下的临时文件，超过 24 小时未被修改的可以安全删除",
			Consequence: "无功能损失；个别正在使用的文件会被自动跳过", allowedRoot: local})
		add(Candidate{ID: "crash_dumps", Title: "应用崩溃转储", Level: LevelSafe, Kind: KindDir,
			Paths: []string{local + `\CrashDumps`}, MinAge: 7 * 24 * time.Hour,
			Reason: "应用崩溃时的内存转储，超过 7 天后诊断价值很低",
			Consequence: "丢失旧的崩溃现场；近 7 天内的会保留", allowedRoot: local})
		add(Candidate{ID: "wer_user", Title: "错误报告（用户）", Level: LevelSafe, Kind: KindDir,
			Paths: []string{local + `\Microsoft\Windows\WER`}, MinAge: 7 * 24 * time.Hour,
			Reason: "Windows 错误报告的队列与归档，属于遥测数据",
			Consequence: "无功能损失", allowedRoot: local})
		add(Candidate{ID: "inet_cache", Title: "系统网络缓存", Level: LevelSafe, Kind: KindDir,
			Paths: []string{local + `\Microsoft\Windows\INetCache`}, MinAge: 0,
			Reason: "IE/系统组件的网络缓存，可随时重建",
			Consequence: "无功能损失；被系统占用的条目会自动跳过", allowedRoot: local})
		add(Candidate{ID: "shell_caches", Title: "资源管理器缓存", Level: LevelSafe, Kind: KindDir,
			Paths: []string{local + `\Microsoft\Windows\Caches`}, MinAge: 0,
			Reason: "资源管理器的内部缓存，可重建",
			Consequence: "无功能损失", allowedRoot: local})
		add(Candidate{ID: "thumb_cache", Title: "缩略图与图标缓存", Level: LevelSafe, Kind: KindFiles,
			Paths: []string{local + `\Microsoft\Windows\Explorer`},
			Patterns: []string{"thumbcache_*.db", "iconcache_*.db"}, MinAge: 0,
			Reason: "图片缩略图与文件图标缓存，可重建",
			Consequence: "清理后首次浏览图片文件夹会变慢；需重启 explorer.exe 才能释放",
			Note:        "建议清理后重启资源管理器或注销一次", allowedRoot: local})
	}

	if profile != "" {
		add(Candidate{ID: "downloads", Title: "下载目录（按文件挑选）", Level: LevelConfirm, Kind: KindDownloads,
			Paths: []string{profile + `\Downloads`}, MinAge: 0,
			Reason: "下载的文件里可能混着三年前的安装包和重要合同，工具无法判断内容价值，必须由你逐个挑选",
			Consequence: "默认全部不勾选；安装包类（>90 天的 exe/msi/zip）会给出删除建议，文档类不提供建议",
			Note:        "展开列表按大小排序", allowedRoot: profile})
	}

	if winRoot != "" {
		add(Candidate{ID: "temp_sys", Title: "系统临时文件", Level: LevelSafe, Kind: KindDir,
			Paths: []string{winRoot + `\Temp`}, MinAge: 24 * time.Hour, AdminRequired: true,
			Reason: "系统与服务留下的临时文件",
			Consequence: "无功能损失；需要管理员权限", allowedRoot: winRoot})
		add(Candidate{ID: "wu_download", Title: "Windows 更新下载缓存", Level: LevelSafe, Kind: KindDir,
			Paths: []string{winRoot + `\SoftwareDistribution\Download`}, MinAge: 0, AdminRequired: true,
			Reason: "已下载的更新安装包，安装完成后残留",
			Consequence: "已安装的更新不受影响；被更新服务占用的文件会自动跳过",
			Note:        "更稳妥的做法是先停止 wuauserv 服务再清；本工具采用保守的「能删多少删多少」策略", allowedRoot: winRoot})
		add(Candidate{ID: "logs_cbs", Title: "组件安装日志", Level: LevelSafe, Kind: KindDir,
			Paths: []string{winRoot + `\Logs\CBS`}, MinAge: 7 * 24 * time.Hour, AdminRequired: true,
			Reason: "组件安装（CBS）日志，超过 7 天后仅存档价值",
			Consequence: "排查历史系统更新问题时少一些日志", allowedRoot: winRoot})
		add(Candidate{ID: "logs_update", Title: "更新日志", Level: LevelSafe, Kind: KindDir,
			Paths: []string{winRoot + `\Logs\WindowsUpdate`}, MinAge: 7 * 24 * time.Hour, AdminRequired: true,
			Reason:      "Windows 更新的 ETL 日志",
			Consequence: "无功能损失", allowedRoot: winRoot})
		add(Candidate{ID: "logs_dism", Title: "DISM 日志", Level: LevelSafe, Kind: KindDir,
			Paths: []string{winRoot + `\Logs\DISM`}, MinAge: 7 * 24 * time.Hour, AdminRequired: true,
			Reason:      "DISM 镜像维护日志",
			Consequence: "无功能损失", allowedRoot: winRoot})
		add(Candidate{ID: "logs_mosetup", Title: "系统安装日志", Level: LevelSafe, Kind: KindDir,
			Paths: []string{winRoot + `\Logs\MoSetup`}, MinAge: 7 * 24 * time.Hour, AdminRequired: true,
			Reason:      "系统升级（MoSetup）日志",
			Consequence: "无功能损失", allowedRoot: winRoot})
		add(Candidate{ID: "live_reports", Title: "内核活动报告", Level: LevelSafe, Kind: KindDir,
			Paths: []string{winRoot + `\LiveKernelReports`}, MinAge: 7 * 24 * time.Hour, AdminRequired: true,
			Reason:      "内核实时报告（可能很大，单文件可达数百 MB）",
			Consequence: "丢失旧的内核诊断数据", allowedRoot: winRoot})
		add(Candidate{ID: "minidump", Title: "小型内存转储", Level: LevelSafe, Kind: KindDir,
			Paths: []string{winRoot + `\Minidump`}, MinAge: 7 * 24 * time.Hour, AdminRequired: true,
			Reason:      "蓝屏时生成的小型转储（MEMORY.DMP 之外的）",
			Consequence: "无法再回看历史蓝屏细节", allowedRoot: winRoot})

		// 以下是需要确认的 L1 项：默认不勾选，由用户逐项开启
		add(Candidate{ID: "prefetch", Title: "预读取数据（Prefetch）", Level: LevelConfirm, Kind: KindDir,
			Paths: []string{winRoot + `\Prefetch`}, MinAge: 0, AdminRequired: true,
			Reason: "加速程序启动的预读取记录；微软不建议手动删",
			Consequence: "删除后一段时间内程序启动变慢（Prefetch 会自行重建），收益通常只有几十 MB",
			allowedRoot: winRoot})
		add(Candidate{ID: "memory_dump", Title: "完整内存转储（MEMORY.DMP）", Level: LevelConfirm, Kind: KindFile,
			Paths: []string{winRoot + `\MEMORY.DMP`}, MinAge: 0, AdminRequired: true,
			Reason:      "蓝屏时的完整内存转储，体积约等于物理内存大小",
			Consequence: "丢失最近一次蓝屏的完整现场；若正在排查蓝屏问题请保留",
			allowedRoot: winRoot})
	}

	// Windows.old：旧系统备份。不在任何禁止根之内（C:\Windows.old），
	// 属于「可删但有代价」的 L1：删除后无法回退到升级前的系统。
	if winRoot != "" && len(winRoot) >= 3 {
		old := winRoot[:3] + `Windows.old`
		if dirOrFileExists(old) {
			out = append(out, Candidate{ID: "windows_old", Title: "旧系统备份（Windows.old）",
				Level: LevelConfirm, Kind: KindDir,
				Paths: []string{old}, MinAge: 0, AdminRequired: true,
				Reason: "系统升级后的回滚备份，Windows 通常在 10 天后自动删除",
				Consequence: "删除后无法退回旧版本系统；若当前系统运行正常再删",
				allowedRoot: winRoot[:3]})
		}
	}

	if programData != "" {
		add(Candidate{ID: "wer_queue", Title: "错误报告队列（系统）", Level: LevelSafe, Kind: KindDir,
			Paths: []string{programData + `\Microsoft\Windows\WER\ReportQueue`,
				programData + `\Microsoft\Windows\WER\ReportArchive`},
			MinAge: 7 * 24 * time.Hour, AdminRequired: true,
			Reason:      "全局错误报告队列与归档",
			Consequence: "无功能损失", allowedRoot: programData})
	}

	// UWP 缓存：Packages 下每个应用容器的 LocalCache 与 TempState 是
	// 约定俗成的缓存位置（docs/design/06 §6.5）；LocalState 等是应用数据
	// 主体，绝不触碰——因为根本不在候选集里，这里只展开这两类子目录。
	if local != "" {
		pkgs := local + `\Packages`
		var uwpPaths []string
		if entries, err := os.ReadDir(pkgs); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				for _, sub := range []string{`LocalCache`, `TempState`} {
					p := filepath.Join(pkgs, e.Name(), sub)
					if dirOrFileExists(p) {
						uwpPaths = append(uwpPaths, p)
					}
				}
			}
		}
		if len(uwpPaths) > 0 {
			out = append(out, Candidate{ID: "uwp_cache", Title: "UWP 应用缓存", Level: LevelSafe, Kind: KindDir,
				Paths: uwpPaths, MinAge: 0,
				Reason: "商店应用的约定缓存位置（LocalCache/TempState），可重建",
				Consequence: "应用首次启动可能变慢；设置与数据（LocalState）不受影响",
				allowedRoot: pkgs})
		}
	}

	return out
}

// envPath 读取环境变量并清理为绝对路径；未设置时返回空串。
func envPath(name string) string {
	v := os.Getenv(name)
	if v == "" {
		return ""
	}
	abs, err := sys.CleanAbsolute(v)
	if err != nil {
		return ""
	}
	return abs
}

func windowsRoot() string {
	root, err := sys.SystemRoot()
	if err != nil {
		return ""
	}
	return strings.TrimRight(root, `\`)
}

func dirOrFileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// 护栏（docs/design/09 §3）：禁止写入的根及其后代，永不接受任何写操作。
// 例外必须显式枚举具体路径，不允许通配。
var forbiddenWriteRoots = []string{
	`C:\Windows`,
	`C:\Program Files`,
	`C:\Program Files (x86)`,
	`C:\ProgramData\Microsoft\Windows\Start Menu`,
	`C:\Recovery`,
	`C:\System Volume Information`,
}

// allowedWriteExceptions 是禁止根内的显式白名单。
// 每一条都必须是某个禁止根的后代（有测试保证），且不含通配符。
var allowedWriteExceptions = []string{
	`C:\Windows\Temp`,
	`C:\Windows\Logs\CBS`,
	`C:\Windows\Logs\DISM`,
	`C:\Windows\Logs\WindowsUpdate`,
	`C:\Windows\Logs\MoSetup`,
	`C:\Windows\SoftwareDistribution\Download`,
	`C:\Windows\LiveKernelReports`,
	`C:\Windows\Minidump`,
	`C:\Windows\Prefetch`,
	`C:\Windows\MEMORY.DMP`, // 单文件；docs/design/06 §4.2：可删但有诊断价值（L1）
}

// GuardCheck 校验目标路径是否在允许写入的范围内。
//
// 候选目录虽是内置白名单，删除动作仍要走这一关：
// 若未来有人往目录表里加错了路径，护栏会把它拦下，而不是把错误放进文件系统。
func GuardCheck(p string) error {
	abs, err := sys.CleanAbsolute(p)
	if err != nil {
		return err
	}
	for _, root := range forbiddenWriteRoots {
		if !sys.IsUnder(abs, root) {
			continue
		}
		// 在禁止根内：只有显式枚举的例外才放行
		for _, ex := range allowedWriteExceptions {
			if sys.IsUnder(abs, ex) {
				return nil
			}
		}
		return fmt.Errorf("路径 %s 位于禁止写入的系统根 %s 内，且不在显式白名单中", abs, root)
	}
	return nil
}
