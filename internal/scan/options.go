package scan

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"

	"winclean/internal/sys"
)

// Options 控制一次扫描的行为。
type Options struct {
	// Roots 是要扫描的根路径（通常是 "C:\"）。必须已经是绝对路径。
	Roots []string

	// MaxDepth 是【上报粒度】上限，不是遍历深度。
	//
	// 重要语义：遍历始终是全深度的，因为父目录的总量必须包含所有后代；
	// 若为了「快」而在 MaxDepth 处停止遍历，父目录的大小就会偏小，
	// 而一个偏小的总量会让用户误以为系统盘还有空间——这比慢更危险。
	// 因此 MaxDepth 只决定「哪些节点值得保留并在报告中展示」。
	MaxDepth int

	// MinSize 是上报的最小占用（字节），用于避免报告被海量小目录淹没。
	MinSize int64

	// Deep 启用深度模式：读取重解析点详情、稀疏/压缩/云占位文件的真实占用。
	Deep bool

	// Workers 是并发 worker 数。
	Workers int

	// Excludes 是完全不遍历的路径前缀（大小不计入任何统计）。
	Excludes []string

	// SummaryOnly 是「只汇总、不展开」的路径前缀。
	// 用于 WinSxS、Windows\Installer 这类：体积巨大、用户有权知道占了多少，
	// 但逐项细分既慢又无意义，且绝不能作为清理目标。
	SummaryOnly []string

	// TopFiles 是要收集的最大文件数量（0 表示不收集）。
	TopFiles int

	// Progress 是调用方提供的进度计数器。
	// 若为 nil，Run 会自行创建一个（调用方就拿不到了，因此交互式场景
	// 应当由 CLI 创建后传入，以便同时渲染进度）。
	Progress *Progress

	// Logf 是可选的日志回调（用于 debug 级别输出）。
	Logf func(format string, args ...any)
}

// 默认值。
const (
	DefaultMaxDepth    = 4
	DefaultMinSize     = 10 * 1024 * 1024 // 10 MB
	DefaultTopFiles    = 30
	MaxWorkers         = 32
)

// WithDefaults 补齐未设置的字段。
func (o Options) WithDefaults() Options {
	if o.MaxDepth <= 0 {
		o.MaxDepth = DefaultMaxDepth
	}
	if o.MinSize <= 0 {
		o.MinSize = DefaultMinSize
	}
	if o.Workers <= 0 {
		o.Workers = runtime.NumCPU() * 2
	}
	if o.Workers > MaxWorkers {
		o.Workers = MaxWorkers
	}
	if o.Workers < 1 {
		o.Workers = 1
	}
	if o.TopFiles < 0 {
		o.TopFiles = 0
	}
	return o
}

// DefaultSummaryOnly 返回这些「只汇总不展开」的系统目录。
//
// 为什么不能完全跳过它们：用户有权知道 WinSxS / Installer 各占了多少 GB ——
// 这往往是「C 盘为什么满了」的重要答案。
// 为什么不能细分：既慢又无意义，而且细粒度信息会诱导用户去删（绝对禁止）。
func DefaultSummaryOnly() []string {
	root, err := sys.SystemRoot()
	if err != nil {
		return nil
	}
	sub := []string{
		`WinSxS`,
		`Installer`,
		`assembly`,
		`servicing`,
		`System32\DriverStore`,
		`System32\CatRoot`,
		`Microsoft.NET`,
		`Prefetch`,
	}
	out := make([]string, 0, len(sub))
	for _, s := range sub {
		out = append(out, root+`\`+s)
	}
	return out
}

// ParseSize 解析人类可读的大小（"10MB"、"1.5GB"、"512KB"、纯数字视为字节）。
//
// 使用 1024 进制（Windows 惯例）。
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("空的大小表达式")
	}

	upper := strings.ToUpper(s)
	multiplier := int64(1)

	switch {
	case strings.HasSuffix(upper, "TB"):
		multiplier = 1024 * 1024 * 1024 * 1024
		s = s[:len(s)-2]
	case strings.HasSuffix(upper, "GB"):
		multiplier = 1024 * 1024 * 1024
		s = s[:len(s)-2]
	case strings.HasSuffix(upper, "MB"):
		multiplier = 1024 * 1024
		s = s[:len(s)-2]
	case strings.HasSuffix(upper, "KB"):
		multiplier = 1024
		s = s[:len(s)-2]
	case strings.HasSuffix(upper, "B"):
		multiplier = 1
		s = s[:len(s)-1]
	}

	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("大小表达式缺少数字")
	}

	// 支持小数（如 1.5GB）
	if strings.Contains(s, ".") {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, fmt.Errorf("无法解析大小 %q: %w", s, err)
		}
		return int64(f * float64(multiplier)), nil
	}

	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("无法解析大小 %q: %w", s, err)
	}
	return n * multiplier, nil
}
