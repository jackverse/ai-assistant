package report

// 决策清单的配套能力：逐项展开核对 + 导出复核。

import (
	"fmt"
	"sort"
	"strings"

	"winclean/internal/model"
)

// BuildDrillDown 把一个目录的直接子项整理成可核对列表。
//
// 用途：用户对某个绿色项不确定时，点开看里面到底有什么——
// 亲眼确认「这确实是临时文件」而不是「我的重要资料」。
// 这是执行删除之前的必要信任来源。
func BuildDrillDown(sub *model.ScanResult) []DecisionItem {
	var items []DecisionItem

	for _, d := range sub.Dirs {
		if d.Depth == 0 || d.IsReparse { // 跳过根自身与链接
			continue
		}
		u := inferUsage(d)
		reason := u.LastUsed + " · " + u.Frequency
		if looksLikeProject(d.Path) {
			reason += " · ⚠️ 含项目文件，不会自动删除"
		}
		if d.Skipped {
			reason += " · 部分未读（权限不足）"
		}
		items = append(items, DecisionItem{
			Name:   humanLabel(d.Path),
			Size:   humanBytes(d.OnDisk),
			Reason: reason,
			Path:   d.Path,
		})
	}

	sort.Slice(items, func(i, j int) bool {
		return parseSizeForSort(items[i].Size) > parseSizeForSort(items[j].Size)
	})
	if len(items) > 40 {
		items = items[:40]
	}
	return items
}

// RenderDecisionMarkdown 把决策清单渲染为 Markdown，用于人工复核与存档。
//
// 刻意带上完整路径：复核判断是否合理时，路径是最重要的依据。
func RenderDecisionMarkdown(groups []DecisionGroup) string {
	var b strings.Builder
	b.WriteString("# 磁盘判断清单\n\n")
	b.WriteString("> 由 winclean 生成，用于人工复核判断是否合理。\n")
	b.WriteString("> 绿色区默认勾选；黄色区默认不勾选；红色区无勾选框。\n\n")

	for _, g := range groups {
		if len(g.Items) == 0 {
			continue
		}
		fmt.Fprintf(&b, "## %s —— %s（合计 %s，%d 项）\n\n",
			g.Title, g.Subtitle, humanBytes(g.TotalBytes), len(g.Items))
		b.WriteString("| 项目 | 大小 | 判断依据 |\n| --- | --- | --- |\n")
		for _, it := range g.Items {
			reason := it.Reason
			if it.MoveHint != "" {
				reason += "；迁移： " + it.MoveHint
			}
			fmt.Fprintf(&b, "| %s | %s | %s |\n", it.Name, it.Size, reason)
		}
		b.WriteString("\n<details><summary>完整路径</summary>\n\n")
		for _, it := range g.Items {
			fmt.Fprintf(&b, "- `%s`\n", it.Path)
		}
		b.WriteString("\n</details>\n\n")
	}

	b.WriteString("---\n\n## 复核提示\n\n")
	b.WriteString("请重点确认绿色区（可以删除）的每一项：\n\n")
	b.WriteString("1. 里面是否真的只有可再生内容（临时文件/缓存/更新残留）\n")
	b.WriteString("2. 是否包含你的代码、文档、聊天记录等不可再生数据\n")
	b.WriteString("3. 若有误判，记下路径与原因，据此收紧判断规则\n")
	return b.String()
}
