package report

// 决策式清单：把扫描结果翻译成「能删 / 需确认 / 不要动」三组。
// 用户只需要勾选和确认，不需要理解路径和分类规则。

import (
	"fmt"
	"sort"
	"strings"

	"winclean/internal/model"
)

// DecisionGroup 是一个色区（能删 / 需确认 / 不要动）。
type DecisionGroup struct {
	Level      string         `json:"level"` // safe / confirm / never
	Title      string         `json:"title"`
	Subtitle   string         `json:"subtitle"`
	Items      []DecisionItem `json:"items"`
	TotalBytes int64          `json:"total_bytes"`
}

// DecisionItem 是一个可操作的条目。
type DecisionItem struct {
	Name     string `json:"name"`                // 人话名称（不是路径）
	Size     string `json:"size"`                // 人类可读大小
	Reason   string `json:"reason"`              // 一句话说清为什么
	MoveHint string `json:"move_hint,omitempty"` // 迁移方法（如果是迁移类）
	Path     string `json:"path"`                // 完整路径（tooltip 展示）
	Checked  bool   `json:"checked"`             // 默认勾选状态
}

// BuildDecisionList 从扫描结果生成三色决策清单。
//
// 这是给前端展示用的最高层接口：
//
//	绿色区（safe）→ 默认全选，用户直接点删除
//	黄色区（confirm）→ 默认不选，用户逐个理解后勾
//	红色区（never）→ 无勾选框，只展示让你知道空间去哪了
//
// codeRoots：用户配置的代码根目录。其下任何路径都不进绿色区——
// 这是对开发者的关键保护（详见 safety.go 的说明）。
func BuildDecisionList(res *model.ScanResult, codeRoots []string) []DecisionGroup {
	type entry struct {
		e        model.DirEntry
		group    string
		reason   string
		moveHint string
		name     string
	}

	var entries []entry
	for _, d := range res.Dirs {
		if d.Depth == 0 || d.OnDisk <= 0 || d.Depth > 4 {
			continue
		}
		if d.OnDisk < 50*1024*1024 { // < 50MB 不进决策清单
			continue
		}
		if d.IsReparse {
			continue
		}

		p := strings.ToLower(d.Path)
		name := humanLabel(d.Path)
		e := entry{e: d, name: name}

		// ① 先跑安全判定：它同时给出「能否进绿色区」与拦截原因
		verdict := safeCleanVerdict(d.Path, codeRoots)
		cat := categorize(d, res.Volumes)

		switch {
		case verdict.Block != "":
			// 被硬保护拦下：一律红区，且说明具体原因
			e.group = "never"
			e.reason = verdict.Block

		case verdict.Safe:
			// 只在【真正的可丢弃位置】才进绿色区
			e.group = "safe"
			e.reason = verdict.Reason

		case cat == GrpDevCache:
			e.group = "confirm"
			e.reason = devCacheReason(p)
			e.moveHint = moveHint(p)

		case cat == GrpUserData:
			e.group = "never"
			e.reason = userDataReason(p)

		case cat == GrpSystem:
			e.group = "never"
			if strings.Contains(p, "pagefile") {
				e.reason = "虚拟内存文件，可在系统设置里改到其他盘"
				e.moveHint = "系统属性 → 高级 → 性能设置 → 虚拟内存"
			} else {
				e.reason = systemReason(p)
			}

		case cat == GrpInstalled:
			e.group = "never"
			e.reason = "已安装的软件，如需卸载请用控制面板"

		default:
			e.group = "never"
			e.reason = "未识别的目录，不确定用途，不会自动处理"
		}

		entries = append(entries, e)
	}

	// 构建三个组
	groups := []DecisionGroup{
		{Level: "safe", Title: "✅ 可以删除", Subtitle: "不影响任何功能"},
		{Level: "confirm", Title: "⚠️ 需要你决定", Subtitle: "有代价但可恢复，建议迁移而非删除"},
		{Level: "never", Title: "🚫 不要动", Subtitle: "系统和重要数据，展示仅让你知道空间去哪了"},
	}

	seen := map[string]bool{} // 去重
	for _, e := range entries {
		key := strings.ToLower(e.e.Path)
		if seen[key] {
			continue
		}
		seen[key] = true

		size := humanBytes(e.e.OnDisk)
		var d *DecisionItem

		switch e.group {
		case "safe":
			d = &DecisionItem{
				Name: e.name, Size: size, Reason: e.reason,
				Path: e.e.Path, Checked: true,
			}
			groups[0].Items = append(groups[0].Items, *d)
			groups[0].TotalBytes += e.e.OnDisk
		case "confirm":
			d = &DecisionItem{
				Name: e.name, Size: size, Reason: e.reason,
				MoveHint: e.moveHint, Path: e.e.Path, Checked: false,
			}
			groups[1].Items = append(groups[1].Items, *d)
			groups[1].TotalBytes += e.e.OnDisk
		case "never":
			d = &DecisionItem{
				Name: e.name, Size: size, Reason: e.reason, Path: e.e.Path,
			}
			groups[2].Items = append(groups[2].Items, *d)
			groups[2].TotalBytes += e.e.OnDisk
		}
	}

	// 每组按大小降序
	for i := range groups {
		g := &groups[i]
		sort.Slice(g.Items, func(a, b int) bool {
			return parseSizeForSort(g.Items[a].Size) > parseSizeForSort(g.Items[b].Size)
		})
	}

	return groups
}

// parseSizeForSort 从 "X.X GB" 格式的字符串提取数值用于排序。
func parseSizeForSort(s string) float64 {
	var v float64
	var unit string
	fmt.Sscanf(s, "%f %s", &v, &unit)
	switch strings.ToUpper(strings.TrimSpace(unit)) {
	case "TB":
		return v * 1024 * 1024
	case "GB":
		return v * 1024
	case "MB":
		return v
	case "KB":
		return v / 1024
	}
	return v
}

func devCacheReason(p string) string {
	p = strings.ToLower(p)
	switch {
	case strings.Contains(p, `npm-cache`) || strings.Contains(p, `yarn`):
		return "删除后下次安装依赖需重新下载（可能耗时较长），建议迁移而非删除"
	case strings.Contains(p, `pip`):
		return "删除后需重新下载 Python 包"
	case strings.Contains(p, `.m2`) || strings.Contains(p, `maven`):
		return "删除后需重新下载所有 Maven 依赖"
	case strings.Contains(p, `docker`):
		return "删除等于删掉所有镜像和容器！强烈建议迁移而非删除"
	case strings.Contains(p, `nvm`):
		return "删除会导致已安装的 Node 版本全部失效"
	case strings.Contains(p, `playwright`) || strings.Contains(p, `puppeteer`):
		return "删除后需重新下载浏览器，下次运行自动化时自动恢复"
	case strings.Contains(p, `jetbrains`) || strings.Contains(p, `intellij`):
		return "IDE 索引缓存，删除后打开项目需重新索引"
	case strings.Contains(p, `android`):
		return "Android SDK，删除后需要重新下载"
	default:
		return "开发工具的缓存或数据，删除可能需要重新配置或下载"
	}
}

func userDataReason(p string) string {
	p = strings.ToLower(p)
	switch {
	case strings.Contains(p, `tencent`) || strings.Contains(p, `wechat`):
		return "聊天记录和接收的文件，删除不可恢复。可在微信内迁移位置"
	case strings.Contains(p, `dingtalk`):
		return "钉钉聊天数据，建议在钉钉内操作"
	case strings.Contains(p, `onedrive`) || strings.Contains(p, `desktop`) || strings.Contains(p, `documents`):
		return "个人文件，绝对不能通过清理工具删除"
	case strings.Contains(p, `cursor`) || strings.Contains(p, `vs code`) || strings.Contains(p, `\code`):
		return "编辑器配置和插件数据"
	default:
		return "用户数据，删除不可恢复"
	}
}

func systemReason(p string) string {
	p = strings.ToLower(p)
	switch {
	case strings.Contains(p, `windows`) && !strings.Contains(p, `temp`):
		return "Windows 系统组件"
	case strings.Contains(p, `program files`):
		return "已安装的程序文件"
	case strings.Contains(p, `pagefile`):
		return "系统虚拟内存文件，可在系统设置里改到其他盘"
	case strings.Contains(p, `hiberfil`):
		return "系统休眠文件，可通过 powercfg /h off 关闭"
	default:
		return "系统组件或受保护的系统数据"
	}
}
