// Package scan 是扫描基础设施：只产出关于文件系统的【事实】，不做任何
// 「能不能删」的判断——那是 analyze/cleanability 的职责。
//
// 边界必须守住，否则规则知识会渗透进遍历代码，导致无法测试、无法复用。
package scan

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"winclean/internal/model"
	"winclean/internal/sys"
	"winclean/internal/version"
	"winclean/internal/winapi"
)

// refineMinSize 保留给未来可能的「只对部分文件做精确查询」策略。
// 当前深度模式对所有非重解析点文件都走句柄精确查询。
const refineMinSize = 0

// reparseDetailLimit 限制读取重解析点详情的目录数量上限。
//
// OneDrive 目录树可能包含数万个占位目录，逐个 CreateFile+DeviceIoControl
// 会显著拖慢扫描。超过上限后只标记「是重解析点」而不再解析目标。
const reparseDetailLimit = 5000

// Progress 是扫描进度的原子计数器。
type Progress struct {
	Files   atomic.Int64
	Dirs    atomic.Int64
	Bytes   atomic.Int64
	Skipped atomic.Int64

	current atomic.Value // string
}

// NewProgress 创建一个进度计数器。
func NewProgress() *Progress { return &Progress{} }

// SetCurrent 记录当前正在处理的目录（用于进度展示）。
func (p *Progress) SetCurrent(path string) { p.current.Store(path) }

// Current 返回当前正在处理的目录。
func (p *Progress) Current() string {
	if v, ok := p.current.Load().(string); ok {
		return v
	}
	return ""
}

// Snapshot 是进度的一次性快照。
type Snapshot struct {
	Files   int64
	Dirs    int64
	Bytes   int64
	Skipped int64
	Current string
}

// Snapshot 取一次快照。
func (p *Progress) Snapshot() Snapshot {
	return Snapshot{
		Files:   p.Files.Load(),
		Dirs:    p.Dirs.Load(),
		Bytes:   p.Bytes.Load(),
		Skipped: p.Skipped.Load(),
		Current: p.Current(),
	}
}

// fileEntry 是「最大文件」列表中的一项，直接复用对外模型以便直接序列化。
type fileEntry = model.LargestFile

// topN 维护按占用排序的前 N 个文件。
//
// 有界内存：只保留 N 项。插入时若已满则与当前最小值比较，仅在更大时替换。
type topN struct {
	max   int
	items []fileEntry
}

func newTopN(max int) *topN { return &topN{max: max} }

func (t *topN) add(e fileEntry) {
	if t.max <= 0 {
		return
	}
	if len(t.items) < t.max {
		t.items = append(t.items, e)
		return
	}
	minIdx := 0
	for i := 1; i < len(t.items); i++ {
		if t.items[i].OnDisk < t.items[minIdx].OnDisk {
			minIdx = i
		}
	}
	if e.OnDisk > t.items[minIdx].OnDisk {
		t.items[minIdx] = e
	}
}

func (t *topN) snapshot() []fileEntry {
	out := make([]fileEntry, len(t.items))
	copy(out, t.items)
	sort.Slice(out, func(i, j int) bool { return out[i].OnDisk > out[j].OnDisk })
	return out
}

// minOnDisk 返回当前列表中的最小占用，以及列表是否已满。
func (t *topN) minOnDisk() (int64, bool) {
	if t.max <= 0 || len(t.items) < t.max {
		return 0, false
	}
	min := t.items[0].OnDisk
	for _, e := range t.items[1:] {
		if e.OnDisk < min {
			min = e.OnDisk
		}
	}
	return min, true
}

// node 是聚合树上的一个目录。
//
// 生命周期：注册（父目录枚举到它时）→ 自身枚举完成 → 所有后代完成 → 回传给父。
// 回传后若不需要保留（!retain）则立即从 map 中删除，以控制内存。
type node struct {
	path  string
	depth int

	parent    string
	hasParent bool

	retain         bool
	retainChildren bool

	outstanding int // 自身 + 未完成的子目录数；归零即代表整棵子树完成
	completed   bool

	logical int64
	ondisk  int64
	files   int64
	dirs    int64 // 含自身

	minMtime int64 // unix 纳秒，0 表示未设置
	maxMtime int64

	isReparse  bool
	tagName    string
	linkTarget string

	hardLinks      int64
	cloudOnly      int64
	skipped        bool
	skipReason     string
	summaryOnly    bool
}

// volumeScanner 扫描单个根，并维护该根的聚合树。
type volumeScanner struct {
	opts  Options
	vol   model.Volume
	prog  *Progress
	stats *scanStats

	// 排除/只汇总的前缀预先转成小写并缓存。
	// 这两个检查对【每个目录】都要做，若在检查内部做 ToLower，
	// 40 万目录 × 8 条规则 × 2 次转换会产生数百万次字符串分配。
	excludesLower []string
	summaryLower  []string

	q *workQueue

	mu      sync.Mutex
	nodes   map[string]*node
	results []*node
	seen    map[fileID]struct{}

	done     chan struct{}
	doneOnce sync.Once
}

// fileID 唯一标识卷上的一个文件（卷序列号 + 文件索引）。
//
// 硬链接必须在此基础上判定，因为硬链接只可能存在于同一卷内。
type fileID struct {
	vol uint32
	idx uint64
}

// scanStats 跨根共享的统计与收集器。
type scanStats struct {
	progress *Progress

	logical           atomic.Int64
	ondisk            atomic.Int64
	errors            atomic.Int64
	reparse           atomic.Int64
	hardLinks         atomic.Int64
	hardLinkFiles     atomic.Int64
	cloudPlaceholders atomic.Int64
	fileIDErrors      atomic.Int64
	maxDepth          atomic.Int64
	reparseDetailOff  atomic.Int64

	// topThreshold 是「进入最大文件列表」的当前门槛（只增不减）。
	// 用于在无锁路径上先做筛选，避免为每个文件去争 top 的互斥锁——
	// 百万级文件时这是主要开销之一。门槛偏小只会多做几次加锁，
	// 不会漏掉真正的大文件，因此无需精确同步。
	topThreshold atomic.Int64

	mu          sync.Mutex
	skippedList []model.SkippedDir
	top         *topN
}

func (s *scanStats) addSkipped(path, reason string) {
	s.mu.Lock()
	// 限制条数，避免权限普遍不足时把内存和 JSON 撑爆
	if len(s.skippedList) < 500 {
		s.skippedList = append(s.skippedList, model.SkippedDir{Path: path, Reason: reason})
	}
	s.mu.Unlock()
}

func (s *scanStats) addTopFile(e fileEntry) {
	s.mu.Lock()
	s.top.add(e)
	if min, full := s.top.minOnDisk(); full {
		// 列表已满，抬高门槛；只在变大时写入，避免来回覆盖。
		updateMax(&s.topThreshold, min)
	}
	s.mu.Unlock()
}

// Run 执行扫描并产出 ScanResult。
//
// 本函数是只读的：不创建、不修改、不删除任何被扫描的数据。
func Run(ctx context.Context, opts Options) (*model.ScanResult, error) {
	opts = opts.WithDefaults()
	startedAt := time.Now()

	vols, err := sys.EnumerateVolumes()
	if err != nil {
		return nil, fmt.Errorf("枚举磁盘失败: %w", err)
	}
	if len(vols) == 0 {
		return nil, fmt.Errorf("未找到任何固定磁盘")
	}

	roots, mode, warnings, err := resolveRoots(opts.Roots, vols)
	if err != nil {
		return nil, err
	}

	stats := &scanStats{top: newTopN(opts.TopFiles)}
	if opts.Progress == nil {
		opts.Progress = NewProgress()
	}
	stats.progress = opts.Progress
	if len(opts.SummaryOnly) == 0 {
		opts.SummaryOnly = DefaultSummaryOnly()
	}
	excludes, err := normalizePrefixes(opts.Excludes)
	if err != nil {
		return nil, err
	}
	opts.Excludes = excludes
	summaryOnly, err := normalizePrefixes(opts.SummaryOnly)
	if err != nil {
		return nil, err
	}
	opts.SummaryOnly = summaryOnly

	var (
		allDirs []model.DirEntry
	)

	for _, root := range roots {
		vol := findVolume(vols, root)
		vs := &volumeScanner{
			opts:          opts,
			vol:           vol,
			prog:          stats.progress,
			stats:         stats,
			excludesLower: lowerAll(opts.Excludes),
			summaryLower:  lowerAll(opts.SummaryOnly),
			q:             newWorkQueue(),
			nodes:         make(map[string]*node),
			seen:          make(map[fileID]struct{}),
			done:          make(chan struct{}),
		}

		nodes, err := vs.scan(ctx, root)
		if err != nil && !errors.Is(err, context.Canceled) {
			warnings = append(warnings, fmt.Sprintf("扫描 %s 时出现问题: %v", root, err))
		}

		for _, n := range nodes {
			allDirs = append(allDirs, nodeToDirEntry(n))
		}
		if ctx.Err() != nil {
			warnings = append(warnings, "扫描被中断，结果不完整")
			break
		}
	}

	// 标记已扫描的卷
	for i := range vols {
		for _, root := range roots {
			if strings.EqualFold(vols[i].Root, root) {
				vols[i].Scanned = true
			}
		}
	}

	// 过滤上报的目录：深度限制 + 最小体积
	allDirs = filterDirs(allDirs, opts)

	res := &model.ScanResult{
		SchemaVersion: model.SchemaVersion,
		ToolVersion:   version.Version,
		ScannedAt:     startedAt,
		DurationMS:    time.Since(startedAt).Milliseconds(),
		Mode:          mode,
		Roots:         roots,
		Volumes:       vols,
		Dirs:          allDirs,
		Stats: model.ScanStats{
			FilesScanned:      stats.progress.Files.Load(),
			DirsScanned:       stats.progress.Dirs.Load(),
			BytesLogical:      stats.logical.Load(),
			BytesOnDisk:       stats.ondisk.Load(),
			SkippedDirs:       stats.progress.Skipped.Load(),
			Errors:            stats.errors.Load(),
			ReparsePoints:     stats.reparse.Load(),
			HardLinksDeduped:  stats.hardLinks.Load(),
			HardLinkFiles:     stats.hardLinkFiles.Load(),
			CloudPlaceholders: stats.cloudPlaceholders.Load(),
			MaxDepthReached:   int(stats.maxDepth.Load()),
			HardLinkDedup:     opts.Deep,
		},
	}

	stats.mu.Lock()
	res.Stats.SkippedReasons = stats.skippedList
	stats.mu.Unlock()

	// 归因完整度：把权威的「卷已用」也带上，供报告对比。
	var usedTotal uint64
	for _, v := range vols {
		for _, r := range roots {
			if strings.EqualFold(v.Root, r) {
				usedTotal += v.UsedBytes
			}
		}
	}
	res.Stats.VolumeUsedBytes = usedTotal

	res.Warnings = append(res.Warnings, warnings...)
	res.Warnings = append(res.Warnings, coverageWarnings(stats, vols, roots, opts.Deep)...)

	// 最大文件列表：挂在 ScanResult 的 Dirs 之外，通过独立的字段暴露。
	res.LargestFiles = stats.top.snapshot()

	return res, nil
}

// resolveRoots 决定要扫描的根，并推断扫描模式。
func resolveRoots(requested []string, vols []model.Volume) ([]string, model.ScanMode, []string, error) {
	var warnings []string

	if len(requested) == 0 {
		sysRoot, err := sys.SystemVolumeRoot()
		if err != nil {
			return nil, "", nil, fmt.Errorf("无法确定系统盘: %w", err)
		}
		return []string{sysRoot}, model.ModeFast, nil, nil
	}

	var cleaned []string
	mode := model.ModeFast

	for _, r := range requested {
		abs, err := sys.CleanAbsolute(r)
		if err != nil {
			return nil, "", nil, fmt.Errorf("无效的扫描根 %q: %w", r, err)
		}
		if sys.IsRootPath(abs) {
			if findVolumeIndex(vols, abs) < 0 {
				warnings = append(warnings, fmt.Sprintf("跳过 %s：不是本机的固定磁盘", abs))
				continue
			}
		} else {
			mode = model.ModeTargeted
		}
		cleaned = append(cleaned, abs)
	}

	if len(cleaned) == 0 {
		return nil, "", nil, fmt.Errorf("没有可扫描的根")
	}

	// 排除嵌套根：C:\ 与 C:\Users 同时扫描会把后者统计两次
	sort.Slice(cleaned, func(i, j int) bool { return len(cleaned[i]) < len(cleaned[j]) })
	var final []string
	for _, c := range cleaned {
		nested := false
		for _, f := range final {
			if f != c && sys.IsUnder(c, f) {
				warnings = append(warnings, fmt.Sprintf("跳过嵌套根 %s：已被 %s 覆盖，重复统计会导致体积翻倍", c, f))
				nested = true
				break
			}
		}
		if !nested {
			final = append(final, c)
		}
	}
	sort.Strings(final)

	return final, mode, warnings, nil
}

func findVolumeIndex(vols []model.Volume, root string) int {
	for i := range vols {
		if strings.EqualFold(vols[i].Root, root) {
			return i
		}
	}
	return -1
}

func findVolume(vols []model.Volume, root string) model.Volume {
	if i := findVolumeIndex(vols, root); i >= 0 {
		return vols[i]
	}
	// 定点扫描某目录而不在已知卷列表内（极少见）：构造一个最小卷信息。
	return model.Volume{Root: sys.VolumeRootOf(root), BytesPerCluster: 4096, FileSystem: "unknown"}
}

// normalizePrefixes 把前缀列表清理为绝对路径。
func normalizePrefixes(in []string) ([]string, error) {
	var out []string
	for _, p := range in {
		abs, err := sys.CleanAbsolute(p)
		if err != nil {
			return nil, fmt.Errorf("无效的排除路径 %q: %w", p, err)
		}
		out = append(out, abs)
	}
	return out, nil
}

// scan 执行单个根的扫描。
func (s *volumeScanner) scan(ctx context.Context, root string) ([]*node, error) {
	s.mu.Lock()
	s.nodes[root] = &node{
		path:           root,
		depth:          0,
		hasParent:      false,
		retain:         true,
		retainChildren: true,
		outstanding:    1,
		dirs:           1,
	}
	s.mu.Unlock()

	s.prog.Dirs.Add(1)
	s.q.watchCancel(ctx)
	s.q.push(task{path: root, depth: 0, retain: true, retainChildren: true})

	var wg sync.WaitGroup
	for i := 0; i < s.opts.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 进度里的「当前目录」只按每 64 个任务更新一次：
			// 每个任务都更新会带来一次字符串的原子存储与分配，
			// 在 40 万目录规模上是可观的开销，而进度展示并不需要那么细。
			local := 0
			for {
				t, ok := s.q.pop(ctx)
				if !ok {
					return
				}
				local++
				if local&63 == 1 {
					s.prog.SetCurrent(t.path)
				}
				s.processDir(ctx, t)
			}
		}()
	}

	wg.Wait()

	return s.results, ctx.Err()
}

// dirAcc 收集单个目录自身的直接统计（不含子目录，子目录由聚合回传）。
type dirAcc struct {
	logical  int64
	ondisk   int64
	files    int64
	hard     int64
	cloud    int64
	minMtime int64
	maxMtime int64
	skipped  bool
	skipMsg  string
}

// processDir 处理一个目录：枚举其内容、登记子目录、累加文件统计。
//
// 无论中途发生什么，都必须让本节点完成（deferred finishDir），
// 否则父节点的 outstanding 永不归零，整个扫描会卡在等待上。
func (s *volumeScanner) processDir(ctx context.Context, t task) {
	acc := &dirAcc{}
	defer s.finishDir(t, acc)

	if ctx.Err() != nil {
		return
	}
	updateMax(&s.stats.maxDepth, int64(t.depth))

	pattern := sys.ToExtendedPath(joinChild(t.path, "*"))
	h, data, err := winapi.FindFirstFileEx(pattern)
	if err != nil {
		// FindFirstFileEx 在【空目录】上返回 ERROR_FILE_NOT_FOUND，这不是错误。
		if errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) {
			return
		}
		s.recordSkipped(t.path, err, acc)
		return
	}
	defer winapi.FindClose(h)

	for {
		// 用原始 UTF-16 数组直接判断 "." 与 ".."，避免为每个条目做字符串转换。
		// 目录项数量是百万级，这里的分配是实打实的开销。
		fn := data.FileName
		if !(fn[0] == '.' && (fn[1] == 0 || (fn[1] == '.' && fn[2] == 0))) {
			s.handleEntry(ctx, t, data, acc)
		}
		if err := winapi.FindNextFile(h, data); err != nil {
			if !errors.Is(err, syscall.ERROR_NO_MORE_FILES) {
				s.stats.errors.Add(1)
			}
			break
		}
	}
}

// handleEntry 处理一个目录项（文件或子目录）。
//
// 性能要点：文件名只在【确实需要】时才从 UTF-16 转换。
// 快速模式下的普通文件（占绝大多数）只需要大小、属性和时间戳，
// 不需要路径字符串。之前对每个条目都做转换，在百万级文件上代价可观。
func (s *volumeScanner) handleEntry(ctx context.Context, t task, data *winapi.Win32FindData, acc *dirAcc) {
	if data.IsDir() {
		name := syscall.UTF16ToString(data.FileName[:])
		childPath := joinChild(t.path, name)

		if data.IsReparsePoint() {
			// 关键安全约束：绝不递归进入重解析点。
			// 否则 Junction 指向 C:\Windows 时会把整棵子树重复统计一遍。
			s.stats.reparse.Add(1)
			retain := t.retainChildren && (t.depth+1 <= s.opts.MaxDepth)
			info := s.readReparseInfo(childPath)
			s.registerReparseLeaf(t.path, childPath, t.depth+1, retain, info)
			return
		}

		if s.isExcluded(childPath) {
			return
		}

		depth := t.depth + 1
		retain := t.retainChildren && depth <= s.opts.MaxDepth

		if s.isSummaryOnly(childPath) {
			// 保留本节点（用户有权知道 WinSxS 占了多少），但不展开子项。
			s.registerChild(t.path, childPath, depth, retain, false, true)
			return
		}

		s.registerChild(t.path, childPath, depth, retain, t.retainChildren, false)
		return
	}

	// ── 文件 ─────────────────────────────────────────────
	size := data.Size()
	ondisk := alignUp(size, s.vol.BytesPerCluster)
	isReparse := data.IsReparsePoint()

	acc.logical += size
	acc.ondisk += ondisk
	acc.files++

	mtime := filetimeToUnix(data.LastWriteTime)
	if mtime != 0 {
		if acc.minMtime == 0 || mtime < acc.minMtime {
			acc.minMtime = mtime
		}
		if mtime > acc.maxMtime {
			acc.maxMtime = mtime
		}
	}

	var reparseTag, linkTarget string

	if isReparse {
		s.stats.reparse.Add(1)
		// 文件级重解析点数量可能极大（OneDrive 占位），只在深度模式解析详情。
		if s.opts.Deep {
			name := syscall.UTF16ToString(data.FileName[:])
			info := s.readReparseInfo(joinChild(t.path, name))
			reparseTag = info.TagName()
			linkTarget = info.Target()
			if info.IsCloud() {
				s.stats.cloudPlaceholders.Add(1)
				acc.cloud++
			}
		}
	}

	// 深度模式：取真实磁盘占用，并做硬链接去重。
	//
	// 两者共用同一个文件句柄（一次 CreateFile 完成两件事），因此成本可控。
	// 为什么必须做：
	//   - 簇对齐估算对 NTFS 驻留文件会高估（小于约 700 字节的文件存于 MFT
	//     记录内，不占簇），AllocationSize 能正确反映这一点；
	//   - 更关键的是硬链接。实测本机 .cache\codex-runtimes 逻辑大小 1127 MB，
	//     实际分配仅 768 MB —— 运行时分发包解压时产生大量硬链接，
	//     不去重会把体积高估约 47%。
	if s.opts.Deep {
		childPath := joinChild(t.path, syscall.UTF16ToString(data.FileName[:]))
		ext := sys.ToExtendedPath(childPath)

		if isReparse {
			// 重解析点（云占位）不打开句柄：避免触发云文件下载。
			// GetCompressedFileSize 只读元数据，是安全的。
			if real, err := winapi.GetCompressedFileSize(ext); err == nil {
				ondisk = int64(real)
				if ondisk == 0 && size > 0 {
					s.stats.cloudPlaceholders.Add(1)
					acc.cloud++
				}
			} else {
				s.stats.fileIDErrors.Add(1)
			}
		} else if md, err := winapi.GetFileMetadata(ext); err == nil {
			if md.Allocation >= 0 {
				ondisk = md.Allocation
			}
			if md.Links > 1 && md.ID.Index != 0 {
				s.stats.hardLinkFiles.Add(1)
				if s.markSeen(fileID{vol: md.ID.VolumeSerial, idx: md.ID.Index}) {
					ondisk = 0
					acc.hard++
				}
			}
		} else {
			s.stats.fileIDErrors.Add(1)
		}

		// 深度模式下可能修正了 ondisk，需要同步修正累加值。
		acc.ondisk += ondisk - alignUp(size, s.vol.BytesPerCluster)

		if s.opts.TopFiles > 0 && ondisk >= s.stats.topThreshold.Load() {
			s.stats.addTopFile(fileEntry{
				Path:       childPath,
				Logical:    size,
				OnDisk:     ondisk,
				Mtime:      mtimePtr(mtime),
				IsReparse:  isReparse,
				ReparseTag: reparseTag,
				LinkTarget: linkTarget,
			})
		}
		return
	}

	// ── 快速模式：不打开任何句柄 ──────────────────────────
	// 只有可能进入「最大文件」列表的文件才需要路径字符串与互斥锁。
	// 通过一个只增不减的阈值先做无锁筛选，避免为每个文件争锁
	// （百万级文件时这是主要开销之一）。
	if s.opts.TopFiles > 0 && size >= s.stats.topThreshold.Load() {
		s.stats.addTopFile(fileEntry{
			Path:       joinChild(t.path, syscall.UTF16ToString(data.FileName[:])),
			Logical:    size,
			OnDisk:     ondisk,
			Mtime:      mtimePtr(mtime),
			IsReparse:  isReparse,
		})
	}
}

// mtimePtr 把 Unix 纳秒时间戳转成指针；0 表示无效，返回 nil。
func mtimePtr(unixNano int64) *time.Time {
	if unixNano == 0 {
		return nil
	}
	t := time.Unix(0, unixNano)
	return &t
}

// markSeen 返回 true 表示该文件之前已经出现过（即当前是一个硬链接）。
func (s *volumeScanner) markSeen(id fileID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.seen[id]; ok {
		s.stats.hardLinks.Add(1)
		return true
	}
	s.seen[id] = struct{}{}
	return false
}

// readReparseInfo 读取重解析点详情，带数量上限保护。
func (s *volumeScanner) readReparseInfo(path string) winapi.ReparseInfo {
	if s.stats.reparse.Load() > reparseDetailLimit {
		s.stats.reparseDetailOff.Add(1)
		return winapi.ReparseInfo{}
	}
	info, err := winapi.ReadReparsePoint(sys.ToExtendedPath(path))
	if err != nil {
		return winapi.ReparseInfo{}
	}
	return info
}

// recordSkipped 记录一个因权限等原因无法读取的目录。
func (s *volumeScanner) recordSkipped(path string, err error, acc *dirAcc) {
	reason := describeError(err)
	acc.skipped = true
	acc.skipMsg = reason

	if errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		s.prog.Skipped.Add(1)
	} else {
		s.stats.errors.Add(1)
	}
	s.stats.addSkipped(path, reason)
}

// describeError 把错误转成给用户看的中文原因。
func describeError(err error) string {
	switch {
	case errors.Is(err, syscall.ERROR_ACCESS_DENIED):
		return "权限不足（以管理员权限运行可读取）"
	case errors.Is(err, syscall.ERROR_FILE_NOT_FOUND), errors.Is(err, syscall.ERROR_PATH_NOT_FOUND):
		return "路径不存在或已被移动"
	case errors.Is(err, sys.ErrCantAccessFile):
		return "云占位文件未下载到本地"
	default:
		return err.Error()
	}
}

// registerChild 登记一个子目录节点并入队处理。
func (s *volumeScanner) registerChild(parentPath, childPath string, depth int, retain, retainChildren, summaryOnly bool) {
	s.mu.Lock()
	if parent := s.nodes[parentPath]; parent != nil {
		parent.outstanding++
	}
	s.nodes[childPath] = &node{
		path:           childPath,
		depth:          depth,
		parent:         parentPath,
		hasParent:      true,
		retain:         retain,
		retainChildren: retainChildren,
		outstanding:    1,
		dirs:           1,
		summaryOnly:    summaryOnly,
	}
	s.mu.Unlock()

	s.prog.Dirs.Add(1)
	s.q.push(task{path: childPath, depth: depth, retain: retain, retainChildren: retainChildren})
}

// registerReparseLeaf 登记一个重解析点目录：不递归进入，登记后立即完成。
func (s *volumeScanner) registerReparseLeaf(parentPath, childPath string, depth int, retain bool, info winapi.ReparseInfo) {
	s.mu.Lock()
	if parent := s.nodes[parentPath]; parent != nil {
		parent.outstanding++
	}
	s.nodes[childPath] = &node{
		path:        childPath,
		depth:       depth,
		parent:      parentPath,
		hasParent:   true,
		retain:      retain,
		outstanding: 1,
		dirs:        1,
		isReparse:   true,
		tagName:     info.TagName(),
		linkTarget:  info.Target(),
	}
	s.mu.Unlock()

	s.prog.Dirs.Add(1)
	s.complete(childPath)
}

// finishDir 把本目录自身的统计写入节点，然后触发完成传播。
//
// 统计累加刻意在这里【按目录批量】做一次，而不是在每个文件上做：
// 快速模式下每文件 4 次全局原子自增（文件数、字节数、逻辑、实际）会让
// 16 个 worker 争抢同一组缓存行，在百万级文件时成为主要瓶颈。
// 批量刷新把原子操作次数从「文件数」降到「目录数」，相差两个数量级。
func (s *volumeScanner) finishDir(t task, acc *dirAcc) {
	s.prog.Files.Add(acc.files)
	s.prog.Bytes.Add(acc.ondisk)
	s.stats.logical.Add(acc.logical)
	s.stats.ondisk.Add(acc.ondisk)

	s.mu.Lock()
	n := s.nodes[t.path]
	if n != nil {
		n.logical += acc.logical
		n.ondisk += acc.ondisk
		n.files += acc.files
		n.hardLinks += acc.hard
		n.cloudOnly += acc.cloud
		n.skipped = acc.skipped
		n.skipReason = acc.skipMsg
		if acc.minMtime != 0 && (n.minMtime == 0 || acc.minMtime < n.minMtime) {
			n.minMtime = acc.minMtime
		}
		if acc.maxMtime > n.maxMtime {
			n.maxMtime = acc.maxMtime
		}
	}
	s.mu.Unlock()

	s.complete(t.path)
}

// complete 递减节点的未完成计数；归零时把统计回传给父节点。
//
// 这是自底向上聚合的核心：节点只有在【所有后代都完成】之后，
// 其数值才是最终值，此时才能安全地累加到父节点。
func (s *volumeScanner) complete(path string) {
	s.mu.Lock()
	n := s.nodes[path]
	if n == nil || n.completed {
		s.mu.Unlock()
		return
	}
	n.outstanding--
	if n.outstanding > 0 {
		s.mu.Unlock()
		return
	}
	n.completed = true

	parentPath := n.parent
	hasParent := n.hasParent
	if n.retain {
		s.results = append(s.results, n)
	}
	delete(s.nodes, path)

	var parent *node
	if hasParent {
		parent = s.nodes[parentPath]
	}

	logical, ondisk, files, dirs := n.logical, n.ondisk, n.files, n.dirs
	minM, maxM := n.minMtime, n.maxMtime
	hard, cloud := n.hardLinks, n.cloudOnly

	if parent != nil {
		parent.logical += logical
		parent.ondisk += ondisk
		parent.files += files
		parent.dirs += dirs
		parent.hardLinks += hard
		parent.cloudOnly += cloud
		if minM != 0 && (parent.minMtime == 0 || minM < parent.minMtime) {
			parent.minMtime = minM
		}
		if maxM > parent.maxMtime {
			parent.maxMtime = maxM
		}
	}
	s.mu.Unlock()

	if parent != nil {
		s.complete(parentPath)
		return
	}

	// 没有父节点：可能是根本身完成，也可能出现了登记缺陷。
	// 两种情况都必须唤醒等待的 worker，否则会死锁。
	s.doneOnce.Do(func() {
		s.q.close()
		close(s.done)
	})
}

// isExcluded 报告路径是否属于「完全不遍历」的排除集。
func (s *volumeScanner) isExcluded(path string) bool {
	return matchesAnyPrefix(path, s.excludesLower)
}

// isSummaryOnly 报告路径是否属于「只汇总不展开」的集合。
func (s *volumeScanner) isSummaryOnly(path string) bool {
	return matchesAnyPrefix(path, s.summaryLower)
}

// matchesAnyPrefix 判断 path 是否等于 prefixes 中某一项或位于其下。
// prefixes 必须预先转为小写；这里只把 path 转一次小写。
func matchesAnyPrefix(path string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return false
	}
	lower := strings.ToLower(strings.TrimRight(path, `\`))
	for _, p := range prefixes {
		if lower == p || strings.HasPrefix(lower, p+`\`) {
			return true
		}
	}
	return false
}

// lowerAll 把路径列表预转为小写。
func lowerAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		out = append(out, strings.ToLower(strings.TrimRight(p, `\`)))
	}
	return out
}

// joinChild 拼接父子路径。
//
// 刻意不用 filepath.Join：后者会调用 Clean，对文件名中的尾随空格/点等
// 历史遗留形态可能产生预期外的改动。这里只做精确拼接。
func joinChild(dir, name string) string {
	if strings.HasSuffix(dir, `\`) {
		return dir + name
	}
	return dir + `\` + name
}

// alignUp 把文件逻辑大小按簇大小向上取整，得到估算的实际占用。
//
// 对稀疏/压缩/云占位文件会高估，因此深度模式会对这类文件查询真实占用。
func alignUp(size int64, cluster uint32) int64 {
	if size <= 0 {
		return 0
	}
	c := int64(cluster)
	if c <= 0 {
		c = 4096
	}
	return (size + c - 1) / c * c
}

// updateMax 用 CAS 循环把 v 并入 *dst 的最大值（无锁）。
func updateMax(dst *atomic.Int64, v int64) {
	for {
		cur := dst.Load()
		if v <= cur {
			return
		}
		if dst.CompareAndSwap(cur, v) {
			return
		}
	}
}

// filetimeToUnix 把 Win32 FILETIME 转成 Unix 纳秒。0 表示时间戳无效。
func filetimeToUnix(ft winapi.Filetime) int64 {
	ns := ft.Nanoseconds()
	if ns == 0 {
		return 0
	}
	return (ns - 116444736000000000) * 100
}

// nodeToDirEntry 把聚合节点转成对外模型。
func nodeToDirEntry(n *node) model.DirEntry {
	e := model.DirEntry{
		Path:        n.path,
		Depth:       n.depth,
		Logical:     n.logical,
		OnDisk:      n.ondisk,
		Files:       n.files,
		Dirs:        n.dirs,
		IsReparse:   n.isReparse,
		ReparseTag:  n.tagName,
		LinkTarget:  n.linkTarget,
		HardLinks:   n.hardLinks,
		CloudOnlyFiles: n.cloudOnly,
		Skipped:     n.skipped,
		SkipReason:  n.skipReason,
		SummaryOnly: n.summaryOnly,
	}
	if n.minMtime != 0 {
		t := time.Unix(0, n.minMtime)
		e.OldestMtime = &t
	}
	if n.maxMtime != 0 {
		t := time.Unix(0, n.maxMtime)
		e.NewestMtime = &t
	}
	return e
}

// filterDirs 按深度与体积过滤上报条目，并按占用降序排序。
func filterDirs(dirs []model.DirEntry, opts Options) []model.DirEntry {
	out := make([]model.DirEntry, 0, len(dirs))
	for _, d := range dirs {
		// 根节点与重解析点始终保留：前者是总量，后者体积极小但信息重要
		if d.Depth == 0 || d.IsReparse {
			out = append(out, d)
			continue
		}
		if d.Depth > opts.MaxDepth {
			continue
		}
		if d.OnDisk < opts.MinSize {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].OnDisk != out[j].OnDisk {
			return out[i].OnDisk > out[j].OnDisk
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// coverageWarnings 生成归因完整度相关的告警。
//
// 主动报告缺口而不是假装数据完整：如果用户看到「总占用」与磁盘实际使用量
// 差距很大（本机约 16 GB 因无管理员权限读不到），会怀疑整个报告的准确性，
// 进而连准确的部分也不信。说清楚反而增强可信度。
func coverageWarnings(stats *scanStats, vols []model.Volume, roots []string, deep bool) []string {
	var warns []string

	if stats.progress.Skipped.Load() > 0 {
		warns = append(warns, fmt.Sprintf(
			"有 %d 个目录因权限不足被跳过，报告的总量偏小。以管理员权限运行可获得更完整结果",
			stats.progress.Skipped.Load()))
	}

	if !deep {
		warns = append(warns, "快速模式未做硬链接去重：NTFS 上被硬链接的文件（WinSxS、部分 Electron 应用）会被重复计入，"+
			"相关目录的体积可能偏高。使用 --deep 可精确去重（较慢）")
	}

	if n := stats.fileIDErrors.Load(); n > 0 {
		warns = append(warns, fmt.Sprintf(
			"有 %d 个文件无法读取文件索引（可能正被占用或为云占位），这些文件未参与硬链接去重", n))
	}

	if n := stats.cloudPlaceholders.Load(); n > 0 {
		warns = append(warns, fmt.Sprintf(
			"检测到 %d 个云占位/稀疏文件，其本地实际占用可能远小于逻辑大小；使用 --deep 可获得精确值", n))
	}

	if n := stats.reparseDetailOff.Load(); n > 0 {
		warns = append(warns, fmt.Sprintf(
			"重解析点数量超过 %d，已停止解析链接目标以控制耗时（仍不会递归进入它们）",
			reparseDetailLimit))
	}

	// 扫描总量与卷实际使用量的对比。
	//
	// 这里刻意不做「差异小就认为完整」的推断：两个方向的误差会互相抵消——
	//   高估：快速模式不去重硬链接、对驻留文件按一个簇计
	//   低估：权限不足跳过的目录（回收站、卷影副本、其他用户 profile）
	// 因此本机实测「扫描 230 GB vs 卷已用 233 GB」看起来只差 1.3%，
	// 实际上缺口与高估各有十几 GB。必须把这个事实讲清楚，
	// 否则用户会因为总量对得上而误以为报告是完全覆盖的。
	scanned := stats.ondisk.Load()
	var usedTotal uint64
	for _, v := range vols {
		for _, r := range roots {
			if strings.EqualFold(v.Root, r) {
				usedTotal += v.UsedBytes
			}
		}
	}
	if usedTotal > 0 && scanned > 0 {
		ratio := (float64(usedTotal) - float64(scanned)) / float64(usedTotal)
		if ratio > 0.05 || ratio < -0.05 {
			warns = append(warns, fmt.Sprintf(
				"扫描统计 %s 与卷实际已用 %s 相差 %.0f%%",
				humanBytes(scanned), humanBytes(int64(usedTotal)), ratio*100))
		}
		if !deep {
			warns = append(warns, fmt.Sprintf(
				"注意：本模式的误差有两个相反方向——硬链接不去重与簇对齐会【高估】，"+
					"权限不足跳过的区域会【低估】。本机两者合计约十几 GB，"+
					"因此「统计值接近卷已用」不代表覆盖完整（有 %d 个目录未读到）",
				stats.progress.Skipped.Load()))
		}
	}

	return warns
}

// humanBytes 是本包内部用的轻量字节格式化（避免依赖 report 包造成层次倒置）。
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
	return fmt.Sprintf("%.1f %s", v, units[i])
}
