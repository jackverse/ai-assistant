// Package report 负责把扫描结果渲染成人可读的形式。
// 本层不做任何计算与判断，只做格式化。
package report

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// Bytes 把字节数格式化为人类可读形式（1024 进制）。
func Bytes(n int64) string {
	const unit = 1024
	if n < 0 {
		return "-" + Bytes(-n)
	}
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	v := float64(n)
	i := -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	// 1 GB 以上保留一位小数更有信息量；1 GB 以下取整避免噪声
	if v >= 100 {
		return fmt.Sprintf("%.0f %s", v, units[i])
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

// Count 把大数字格式化为带千位分隔符的形式。
func Count(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
		if len(s) > pre {
			b.WriteString(",")
		}
	}
	for i := pre; i < len(s); i += 3 {
		b.WriteString(s[i : i+3])
		if i+3 < len(s) {
			b.WriteString(",")
		}
	}
	return b.String()
}

// Percent 格式化百分比。
func Percent(part, total int64) string {
	if total <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", float64(part)/float64(total)*100)
}

// Duration 格式化耗时。
func Duration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// NewTable 创建一个对齐的表格写入器。
func NewTable(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
}

// Section 打印一个小节标题。
func Section(w io.Writer, title string) {
	fmt.Fprintf(w, "\n%s\n", title)
	fmt.Fprintln(w, strings.Repeat("─", displayWidth(title)))
}

// displayWidth 估算字符串的终端显示宽度（中文按 2 列计）。
func displayWidth(s string) int {
	width := 0
	for _, r := range s {
		if isWide(r) {
			width += 2
		} else {
			width++
		}
	}
	if width < 8 {
		width = 8
	}
	return width
}

// isWide 判断字符是否为全角（粗略但足够用于对齐）。
func isWide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F, // 韩文字母
		r >= 0x2E80 && r <= 0xA4CF, // CJK 部首、假名、汉字
		r >= 0xAC00 && r <= 0xD7A3, // 韩文音节
		r >= 0xF900 && r <= 0xFAFF, // CJK 兼容汉字
		r >= 0xFE30 && r <= 0xFE6F, // CJK 兼容形式
		r >= 0xFF00 && r <= 0xFF60, // 全角形式
		r >= 0xFFE0 && r <= 0xFFE6:
		return true
	}
	return false
}
