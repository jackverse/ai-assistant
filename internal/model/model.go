// Package model 定义全项目共享的数据模型。
//
// 本包【零依赖】：不 import 任何 internal/* 子包，以保证任何层都能安全引用
// 而不产生循环依赖。字段与 docs/design/08-数据模型与CLI.md 保持一致。
package model

import "time"

// SchemaVersion 是 scan.json 的格式版本，便于后续兼容演进。
const SchemaVersion = "1.0"

// ScanMode 是扫描模式。
type ScanMode string

const (
	// ModeFast 快速模式：只按簇对齐估算实际占用，不读重解析点详情。
	ModeFast ScanMode = "fast"
	// ModeDeep 深度模式：额外读取重解析点详情、稀疏/压缩文件的真实占用、mtime 分布。
	ModeDeep ScanMode = "deep"
	// ModeTargeted 定点模式：只扫指定目录。
	ModeTargeted ScanMode = "targeted"
)

// DriveType 是驱动器类型（字符串形式，便于 JSON 阅读）。
type DriveType string

const (
	DriveFixed     DriveType = "fixed"
	DriveRemovable DriveType = "removable"
	DriveRemote    DriveType = "remote"
	DriveCDROM     DriveType = "cdrom"
	DriveRAMDisk   DriveType = "ramdisk"
	DriveOther     DriveType = "other"
)

// Volume 描述一个磁盘卷。
type Volume struct {
	Root            string    `json:"root"`             // "C:\"
	Letter          string    `json:"letter"`           // "C"
	Label           string    `json:"label,omitempty"`  // 卷标
	FileSystem      string    `json:"file_system"`      // "NTFS"
	SerialNumber    uint32    `json:"serial_number"`
	TotalBytes      uint64    `json:"total_bytes"`
	FreeBytes       uint64    `json:"free_bytes"`
	UsedBytes       uint64    `json:"used_bytes"`       // 计算值 = Total - Free
	BytesPerCluster uint32    `json:"bytes_per_cluster"`
	DriveType       DriveType `json:"drive_type"`
	IsNTFS          bool      `json:"is_ntfs"`          // 迁移预检依赖此字段
	IsSystem        bool      `json:"is_system"`        // 是否为 Windows 所在卷
	IsBoot          bool      `json:"is_boot"`
	Scanned         bool      `json:"scanned"`
}

// UsedPercent 返回使用率（0-100）。
func (v Volume) UsedPercent() float64 {
	if v.TotalBytes == 0 {
		return 0
	}
	return float64(v.UsedBytes) / float64(v.TotalBytes) * 100
}

// DirEntry 是扫描产出的一条目录记录。
//
// 注意 Logical 与 OnDisk 的区别：
//   - Logical 是文件声明的字节数之和，符合用户的直觉
//   - OnDisk 是簇对齐后的实际占用，反映真实能释放多少空间
//
// 两者在稀疏文件（VHDX）、云占位文件上差异巨大，报告「可释放 X GB」必须用 OnDisk。
type DirEntry struct {
	Path  string `json:"path"`
	Depth int    `json:"depth"`

	Logical int64 `json:"logical"`
	OnDisk  int64 `json:"on_disk"`
	Files   int64 `json:"files"`
	Dirs    int64 `json:"dirs"` // 含自身

	OldestMtime *time.Time `json:"oldest_mtime,omitempty"`
	NewestMtime *time.Time `json:"newest_mtime,omitempty"`

	// 重解析点信息。此类目录不被递归进入，因此 Logical/OnDisk 为 0。
	IsReparse  bool   `json:"is_reparse,omitempty"`
	ReparseTag string `json:"reparse_tag,omitempty"`
	LinkTarget string `json:"link_target,omitempty"`

	// HardLinks 是被去重跳过的硬链接数量（这些文件的占用只计一次）。
	HardLinks int64 `json:"hard_links,omitempty"`

	// CloudOnlyFiles 是云占位文件数量（本地实际占用可能为 0）。
	CloudOnlyFiles int64 `json:"cloud_only_files,omitempty"`

	// Skipped 表示该目录因权限等原因未能读取，其大小为不完全值。
	Skipped    bool   `json:"skipped,omitempty"`
	SkipReason string `json:"skip_reason,omitempty"`

	// SummaryOnly 表示该目录只做汇总、不展开子项（如 WinSxS、Windows\Installer）。
	SummaryOnly bool `json:"summary_only,omitempty"`
}

// SkippedDir 记录一个被跳过的目录及原因。
type SkippedDir struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// ScanStats 是扫描过程的统计。
type ScanStats struct {
	FilesScanned      int64 `json:"files_scanned"`
	DirsScanned       int64 `json:"dirs_scanned"`
	BytesLogical      int64 `json:"bytes_logical"`
	BytesOnDisk       int64 `json:"bytes_on_disk"`
	SkippedDirs       int64 `json:"skipped_dirs"`
	Errors            int64 `json:"errors"`
	ReparsePoints     int64 `json:"reparse_points"`
	HardLinksDeduped  int64 `json:"hard_links_deduped"`
	HardLinkFiles     int64 `json:"hard_link_files"`
	CloudPlaceholders int64 `json:"cloud_placeholders"`
	MaxDepthReached   int   `json:"max_depth_reached"`

	// HardLinkDedup 表明本次扫描是否启用了硬链接去重。
	// 只有 --deep 模式才启用（需要逐文件调用 GetFileInformationByHandle）。
	// 明确暴露这个事实，避免使用者把未去重的体积误当成精确值。
	HardLinkDedup bool `json:"hard_link_dedup"`

	// VolumeUsedBytes 是被扫描各卷【实际已用】的字节数之和（来自卷信息，权威值）。
	// 与 BytesOnDisk 对比可看出统计完整度。
	VolumeUsedBytes uint64 `json:"volume_used_bytes"`

	// SkippedReasons 记录被跳过的目录及原因。
	SkippedReasons []SkippedDir `json:"skipped_reasons,omitempty"`
}

// LargestFile 是「最大文件」列表中的一项。
//
// 这个列表的价值很高：C 盘最大的几项往往就是单个巨型文件
// （Docker 的 docker_data.vhdx、pagefile.sys、hiberfil.sys），
// 它们在目录维度上只会表现为「C:\ 很大」，必须按文件维度才能看见。
type LargestFile struct {
	Path       string     `json:"path"`
	Logical    int64      `json:"logical"`
	OnDisk     int64      `json:"on_disk"`
	Mtime      *time.Time `json:"mtime,omitempty"`
	IsReparse  bool       `json:"is_reparse,omitempty"`
	ReparseTag string     `json:"reparse_tag,omitempty"`
	LinkTarget string     `json:"link_target,omitempty"`
}

// ScanResult 是扫描的完整产出，也是后续所有命令的唯一输入。
type ScanResult struct {
	SchemaVersion string    `json:"schema_version"`
	ToolVersion   string    `json:"tool_version"`
	ScannedAt     time.Time `json:"scanned_at"`
	DurationMS    int64     `json:"duration_ms"`
	Mode          ScanMode  `json:"mode"`

	Roots   []string  `json:"roots"`
	Volumes []Volume  `json:"volumes"`
	Dirs    []DirEntry `json:"dirs"`

	// LargestFiles 是占用最大的若干文件（数量由 --top-files 控制）。
	LargestFiles []LargestFile `json:"largest_files,omitempty"`

	// 以下字段由后续里程碑填充（见 docs/design/11-里程碑与验收.md）。
	// M1 阶段为空，用 omitempty 避免在 JSON 中产生误导性的空数组。
	Apps         []any `json:"apps,omitempty"`
	Findings     []any `json:"findings,omitempty"`
	Paths        []any `json:"paths,omitempty"`
	Migrations   []any `json:"migration_candidates,omitempty"`
	SystemTweaks []any `json:"system_tweaks,omitempty"`
	KnownFolders []any `json:"known_folders,omitempty"`

	Stats    ScanStats `json:"stats"`
	Warnings []string  `json:"warnings,omitempty"`
}
