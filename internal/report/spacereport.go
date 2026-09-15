package report

// 空间报告：把扫描事实翻译成人能读懂并直接做决定的语言。
//
// 设计文档见 docs/design/17-空间报告设计.md。
// 核心：不是按「文件在哪」分组，而是按「你能做什么」分组。

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"winclean/internal/model"
)

// ───────── 报告模型 ─────────

type SpaceReport struct {
	Groups    []ReportGroup `json:"groups"`
	AIEnabled bool          `json:"ai_enabled"`
	AINote    string        `json:"ai_note"`
	Summary   ReportSummary `json:"summary"`
}

type ReportSummary struct {
	TotalUsed    string `json:"total_used"`
	TotalFree    string `json:"total_free"`
	CanMove      string `json:"can_move"`
	CanClean     string `json:"can_clean"`
	UserData     string `json:"user_data"`
	UnknownCount int    `json:"unknown_count"`
}

type ReportGroup struct {
	ID         string       `json:"id"`
	Title      string       `json:"title"`
	Subtitle   string       `json:"subtitle"`
	Icon       string       `json:"icon"`
	Action     string       `json:"action"` // "不要动" / "可迁移 X GB" / "可删" / "按需处理"
	Items      []ReportItem `json:"items"`
	TotalBytes int64        `json:"total_bytes"`
}

type ReportItem struct {
	Label       string `json:"label"`
	Detail      string `json:"detail"`
	SizeHuman   string `json:"size_human"`
	LastUsed    string `json:"last_used"`
	Frequency   string `json:"frequency"`
	CanMove     bool   `json:"can_move"`
	MoveHint    string `json:"move_hint,omitempty"`
	CanDelete   bool   `json:"can_delete"`
	DeleteHint  string `json:"delete_hint,omitempty"`
	Path        string `json:"path"`
}

// ───────── 分类 ─────────

const (
	GrpSystem    = "system"
	GrpInstalled = "installed"
	GrpDevCache  = "dev-cache"
	GrpUserData  = "user-data"
	GrpSafeClean = "safe-clean"
	GrpUnknown   = "unknown"
)

var groupMeta = map[string]struct{ title, subtitle, icon, action string }{
	GrpSystem:    {"系统组件", "不要动", "🪟", "按需处理"},
	GrpInstalled: {"已安装软件", "按需处理", "📦", "按需处理"},
	GrpDevCache:  {"开发工具与缓存", "通常可以搬到其他盘", "🔧", "可迁移"},
	GrpUserData:  {"用户数据", "不要删", "📁", "不要动"},
	GrpSafeClean: {"可安全清理", "删了不影响功能", "🧹", "可删"},
	GrpUnknown:   {"没认出来的", "需要你人工判断", "❓", "需要确认"},
}

// Categorize 把一个扫描条目分到某个组。
//
// 优先级从高到低：用户数据 > 系统 > IM 数据 > 可安全清理 > 开发缓存 > 已安装 > 未知
func categorize(e model.DirEntry, volumes []model.Volume) string {
	p := strings.ToLower(e.Path)

	// 用户数据优先：Known Folder 与 IM 聊天记录绝对不能建议删除
	for _, kw := range []string{
		`onedrive`, `documents`, `desktop`, `pictures`, `videos`, `music`,
		`tencent`, `wechat`, `wechat files`, `dingtalk`, `feishu`, `larkshell`,
	} {
		if strings.Contains(p, kw) {
			return GrpUserData
		}
	}

	// 系统区域
	for _, kw := range []string{
		`c:\windows`, `c:\program files`, `c:\programdata\microsoft`,
		`pagefile.sys`, `hiberfil.sys`, `swapfile.sys`, `config.msi`,
		`$recycle.bin`, `system volume information`,
	} {
		if strings.Contains(p, kw) {
			return GrpSystem
		}
	}

	// 可安全清理：只收真正无害的（临时文件、崩溃转储、更新残留）
	// 注意：npm-cache/yarn-cache/pip-cache 放在 dev-cache，因为它们的
	// 更好出路是【迁移到 D 盘】而非删除——删了要重新下载
	for _, kw := range []string{
		`\temp`, `-updater`, ` installer`, `crashdumps`,
		`softwaredistribution\download`, `\crashreport`,
	} {
		if strings.Contains(p, kw) {
			return GrpSafeClean
		}
	}

	// 开发工具与缓存：包管理器、运行时、构建产物
	for _, kw := range []string{
		`\.cache`, `\.m2`, `\.gradle`, `\.npm`, `\.cargo`, `\.nuget`,
		`\npm-cache`, `\yarn\cache`, `\pip\cache`,
		`\nvm`, `\docker`, `\android\sdk`, `\.conda`,
		`\golang`, `\go\`, `\python`, `\java\`,
		`ms-playwright`, `cypress`, `puppeteer`, `electron`,
		`node_modules`, `\.vscode\extensions`,
		`jetbrains`, `intelliji`, `\cursor\`,
	} {
		if strings.Contains(p, kw) {
			return GrpDevCache
		}
	}

	// Program Files 下的安装软件
	if strings.Contains(p, `program files`) || strings.Contains(p, `programdata`) {
		return GrpInstalled
	}

	return GrpUnknown
}

// ───────── 使用推断 ─────────

// UsageInfo 是从 mtime 推断的使用情况。
type UsageInfo struct {
	LastUsed  string // "今天" / "3 天前" / "超过 60 天"
	Frequency string // "常用" / "近期用过" / "闲置"
}

func inferUsage(e model.DirEntry) UsageInfo {
	if e.NewestMtime == nil {
		return UsageInfo{LastUsed: "—", Frequency: "—"}
	}
	days := int(time.Since(*e.NewestMtime).Hours() / 24)

	var last string
	switch {
	case days <= 0:
		last = "今天"
	case days == 1:
		last = "昨天"
	case days < 30:
		last = fmt.Sprintf("%d 天前", days)
	case days < 365:
		last = fmt.Sprintf("%d 个月前", days/30)
	default:
		last = fmt.Sprintf("超过 1 年")
	}

	var freq string
	switch {
	case days < 7:
		freq = "常用"
	case days < 30:
		freq = "近期用过"
	default:
		freq = "闲置"
	}

	return UsageInfo{LastUsed: last, Frequency: freq}
}

// ───────── 人类可读标签 ─────────

// humanLabel 把路径翻译成短标签。
//
// 规则：取路径最后 1–2 个有意义的段，映射常见名称。
func humanLabel(path string) string {
	p := strings.ToLower(path)

	known := map[string]string{
		`docker_data.vhdx`:    "Docker 数据盘",
		`pagefile.sys`:        "系统页面文件（虚拟内存）",
		`hiberfil.sys`:        "系统休眠文件",
		`\yarn\cache`:         "Yarn 包缓存",
		`npm-cache`:           "npm 包缓存",
		`.m2`:                 "Maven 本地仓库",
		`puppeteer`:           "Puppeteer 浏览器缓存",
		`ms-playwright`:       "Playwright 浏览器缓存",
		`codex-runtimes`:      "Codex 运行时缓存",
		`tencent`:             "腾讯系数据（聊天记录/文件）",
		`\cursor`:             "Cursor 编辑器",
		`\code`:               "VS Code",
		`intellijidea2024.3`:  "IntelliJ IDEA 2024.3",
		`intellijidea2022.2`:  "IntelliJ IDEA 2022.2（旧版）",
		`dingtalk`:            "钉钉",
		`larkshell`:           "飞书",
		`baidunetdisk`:        "百度网盘",
		`discord`:             "Discord",
		`kingsoft`:            "WPS Office",
		`wic-admin`:           "wic-admin 模块",
		`wic-pub`:             "wic-pub 模块",
	}
	if label, ok := known[p]; ok {
		return label
	}
	// 路径末段
	segs := strings.Split(strings.TrimRight(path, `\`), `\`)
	name := segs[len(segs)-1]
	if name == "" && len(segs) > 1 {
		name = segs[len(segs)-2]
	}
	// 首字母大写（如果全小写英文）
	if name != "" && name[0] >= 'a' && name[0] <= 'z' {
		name = strings.ToUpper(name[:1]) + name[1:]
	}
	return name
}

// ───────── 报告构建 ─────────

// BuildSpaceReport 从扫描结果构建人类可读的空间报告。
func BuildSpaceReport(res *model.ScanResult) SpaceReport {
	var groups []ReportGroup
	groupIdx := map[string]int{}

	getGroup := func(id string) *ReportGroup {
		if idx, ok := groupIdx[id]; ok {
			return &groups[idx]
		}
		meta := groupMeta[id]
		g := ReportGroup{
			ID: id, Title: meta.title, Subtitle: meta.subtitle,
			Icon: meta.icon, Action: meta.action,
		}
		groups = append(groups, g)
		groupIdx[id] = len(groups) - 1
		return &groups[len(groups)-1]
	}

	var unknownCount int
	var totalCanMove, totalCanClean, totalUserData int64

	for _, d := range res.Dirs {
		if d.Depth == 0 || d.OnDisk <= 0 {
			continue
		}
		if d.Depth > 4 {
			continue
		}
		if d.OnDisk < 100*1024*1024 { // < 100MB 不进报告
			continue
		}

		group := categorize(d, res.Volumes)
		usage := inferUsage(d)
		g := getGroup(group)

		item := ReportItem{
			Label: humanLabel(d.Path),
			SizeHuman: humanBytes(d.OnDisk),
			LastUsed:  usage.LastUsed,
			Frequency: usage.Frequency,
			Path:      d.Path,
		}

		// 迁移与删除建议
		switch group {
		case GrpDevCache:
			item.CanMove = true
			item.MoveHint = moveHint(d.Path)
			item.CanDelete = true
			item.DeleteHint = "删除后下次使用需重新下载"
			totalCanMove += d.OnDisk
		case GrpSafeClean:
			item.CanDelete = true
			item.DeleteHint = "可安全删除"
			totalCanClean += d.OnDisk
		case GrpUserData:
			totalUserData += d.OnDisk
		case GrpUnknown:
			unknownCount++
		}

		if d.IsReparse {
			item.Label += "（链接 → " + d.LinkTarget + "）"
		}
		if d.SummaryOnly {
			item.Detail = "仅汇总，不展开子项"
		}

		g.Items = append(g.Items, item)
		g.TotalBytes += d.OnDisk
	}

	// 更新组的 Action 与排序
	for i := range groups {
		g := &groups[i]
		switch g.ID {
		case GrpDevCache:
			g.Action = fmt.Sprintf("可迁移 %.1f GB", float64(g.TotalBytes)/(1<<30))
			totalCanMove = g.TotalBytes
		case GrpSafeClean:
			g.Action = fmt.Sprintf("可删 %.1f GB", float64(g.TotalBytes)/(1<<30))
			totalCanClean = g.TotalBytes
		case GrpUserData:
			g.Action = "不要删"
			totalUserData = g.TotalBytes
		}
		// 按大小降序
		sort.Slice(g.Items, func(a, b int) bool {
			return g.Items[a].SizeHuman != g.Items[b].SizeHuman
		})
	}

	// 组排序：按用户关注度（可清理 → 可迁移 → 已安装 → 用户数据 → 系统 → 未知）
	order := map[string]int{
		GrpSafeClean: 0, GrpDevCache: 1, GrpInstalled: 2,
		GrpUserData: 3, GrpSystem: 4, GrpUnknown: 5,
	}
	sort.Slice(groups, func(a, b int) bool {
		return order[groups[a].ID] < order[groups[b].ID]
	})

	// 卷概览
	var used, free int64
	for _, v := range res.Volumes {
		if v.Scanned {
			used = int64(v.UsedBytes)
			free = int64(v.FreeBytes)
		}
	}

	return SpaceReport{
		Groups: groups,
		Summary: ReportSummary{
			TotalUsed:    humanBytes(int64(used)),
			TotalFree:    humanBytes(int64(free)),
			CanMove:      humanBytes(totalCanMove),
			CanClean:     humanBytes(totalCanClean),
			UserData:     humanBytes(totalUserData),
			UnknownCount: unknownCount,
		},
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	v := float64(n)
	i := -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	if v >= 100 {
		return fmt.Sprintf("%.0f %s", v, units[i])
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

// moveHint 根据路径推断迁移建议。
func moveHint(path string) string {
	p := strings.ToLower(path)
	hints := []struct{ kw, hint string }{
		{`yarn`, "设置环境变量 YARN_CACHE_FOLDER 到 D 盘"},
		{`npm-cache`, "设置环境变量 NPM_CONFIG_CACHE 到 D 盘"},
		{`pip`, "设置环境变量 PIP_CACHE_DIR 到 D 盘"},
		{`.m2`, "修改 Maven settings.xml 的 localRepository 到 D 盘"},
		{`nvm`, "改 NVM_HOME 环境变量到 D 盘"},
		{`docker`, "Docker Desktop 设置里改 Disk image location"},
		{`android`, "设 ANDROID_SDK_ROOT 到 D 盘，同步更新项目 local.properties"},
		{`puppeteer`, "设 PUPPETEER_CACHE_DIR 到 D 盘"},
		{`playwright`, "设 PLAYWRIGHT_BROWSERS_PATH 到 D 盘"},
		{`cypress`, "设 CYPRESS_CACHE_FOLDER 到 D 盘"},
		{`.cache`, "对该目录建 Junction 到 D 盘，或设 XDG_CACHE_HOME"},
	}
	for _, h := range hints {
		if strings.Contains(p, h.kw) {
			return h.hint
		}
	}
	return "对该目录创建 Junction（目录联接）到 D 盘"
}

// GrpDevCache 等常量导出，供模板/前端比对。
var (
	_ = GrpSystem
	_ = GrpInstalled
	_ = GrpDevCache
	_ = GrpUserData
	_ = GrpSafeClean
	_ = GrpUnknown
)
