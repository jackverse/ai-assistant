# 08 — 数据模型与 CLI 接口

本篇是实现的直接依据：结构体定义、JSON 格式、命令树。

---

## 1. 设计原则

1. **`model` 包零依赖**：只定义类型，不 import 任何 `internal/*`。保证任何层都能安全引用，不会产生循环依赖。
2. **`scan.json` 是唯一事实来源**：所有后续命令（报告、计划、查询）都从它读取，绝不重新扫描。
3. **JSON 里的一切都带版本**：`schema_version` 保证后续演进时能兼容处理。
4. **所有大小字段用 `int64` 字节**：不用 MB/GB 浮点，避免精度与单位混乱。展示时才格式化。
5. **所有等级与类型用字符串枚举**：JSON 可读，且新增枚举值不会破坏旧消费者（相比整数枚举）。

---

## 2. 核心模型

### 2.1 扫描结果根对象

```go
// model/scan.go
const SchemaVersion = "1.0"

type ScanResult struct {
    SchemaVersion string     `json:"schema_version"`
    ToolVersion   string     `json:"tool_version"`
    ScannedAt     time.Time  `json:"scanned_at"`
    DurationMS    int64      `json:"duration_ms"`
    Mode          ScanMode   `json:"mode"`         // fast / deep / targeted
    Volumes       []Volume   `json:"volumes"`
    Apps          []App      `json:"apps"`
    Findings      []Finding  `json:"findings"`
    Paths         []PathEntry `json:"paths"`
    Migrations    []MigrationCandidate `json:"migration_candidates"`
    SystemTweaks  []SystemTweak        `json:"system_tweaks"`
    KnownFolders  []KnownFolder        `json:"known_folders"`
    Stats         ScanStats  `json:"stats"`
    Warnings      []string   `json:"warnings"`
}
```

### 2.2 卷

```go
// model/volume.go
type Volume struct {
    Root            string `json:"root"`              // "C:\"
    Letter          string `json:"letter"`            // "C"
    Label           string `json:"label"`             // "Windows-SSD"
    FileSystem      string `json:"file_system"`       // "NTFS"
    SerialNumber    uint32 `json:"serial_number"`
    TotalBytes      uint64 `json:"total_bytes"`
    FreeBytes       uint64 `json:"free_bytes"`
    UsedBytes       uint64 `json:"used_bytes"`        // 计算值
    BytesPerCluster uint32 `json:"bytes_per_cluster"`
    DriveType       string `json:"drive_type"`        // fixed/removable/remote/cdrom
    IsSystem        bool   `json:"is_system"`
    IsBoot          bool   `json:"is_boot"`
    Scanned         bool   `json:"scanned"`
    IsNTFS          bool   `json:"is_ntfs"`           // 迁移预检依赖此字段
}
```

### 2.3 软件

```go
// model/app.go
type Category string
const (
    CatSystem      Category = "system"
    CatBrowser     Category = "browser"
    CatDevTool     Category = "devtool"
    CatAITool      Category = "ai-tool"
    CatIM          Category = "im"
    CatOffice      Category = "office"
    CatGame        Category = "game"
    CatMedia       Category = "media"
    CatDownloadTool Category = "download-tool"
    CatSecurity    Category = "security"
    CatRuntime     Category = "runtime"
    CatUtility     Category = "utility"
    CatUnknown     Category = "unknown"
)

type AppSource string
const (
    SrcHKLM64    AppSource = "registry-hklm64"
    SrcHKLM32    AppSource = "registry-hklm32"
    SrcHKCU      AppSource = "registry-hkcu"
    SrcUWP       AppSource = "uwp"
    SrcPortable  AppSource = "portable"
    SrcGameStore AppSource = "gamestore"
)

type App struct {
    // 标识
    ID        string `json:"id,omitempty"`         // 规则库 ID，如 "google.chrome"
    Name      string `json:"name"`                 // 规范化显示名
    RawName   string `json:"raw_name,omitempty"`   // 注册表原始值
    Publisher string `json:"publisher,omitempty"`
    Version   string `json:"version,omitempty"`

    // 来源
    Sources []AppSource `json:"sources"`

    // 位置
    InstallLocation       string `json:"install_location,omitempty"`
    InstallLocationSource string `json:"install_location_source,omitempty"`
    //   displayicon / uninstallstring / quietuninstallstring / msi / heuristic / notfound
    InstallVolume string `json:"install_volume,omitempty"`   // "C" / "D"

    // 知识（来自规则库）
    Category        Category `json:"category"`
    Purpose         string   `json:"purpose,omitempty"`
    Homepage        string   `json:"homepage,omitempty"`
    MatchedRule     string   `json:"matched_rule,omitempty"`
    MatchConfidence float64  `json:"match_confidence"`

    // 空间（来自扫描归因）
    Dirs         []DirUsage `json:"dirs"`
    TotalOnDisk  int64      `json:"total_on_disk"`
    TotalLogical int64      `json:"total_logical"`

    // 状态
    Orphaned     bool   `json:"orphaned,omitempty"`
    Filtered     bool   `json:"filtered,omitempty"`
    FilterReason string `json:"filter_reason,omitempty"`
}

type DirUsage struct {
    Path       string   `json:"path"`
    Kind       PathKind `json:"kind"`
    OnDisk     int64    `json:"on_disk"`
    Logical    int64    `json:"logical"`
    FileCount  int64    `json:"file_count"`
    Exists     bool     `json:"exists"`
    DiscoveredBy string `json:"discovered_by"`  // rule / knownfolder / env / registry / heuristic
    RuleID     string   `json:"rule_id,omitempty"`
    Confidence float64  `json:"confidence"`
    Note       string   `json:"note,omitempty"`
}
```

### 2.4 路径

```go
// model/path.go
type PathKind string
const (
    KindInstall  PathKind = "install"
    KindCache    PathKind = "cache"
    KindData     PathKind = "data"
    KindConfig   PathKind = "config"
    KindDownload PathKind = "download"
    KindLog      PathKind = "log"
    KindTemp     PathKind = "temp"
    KindDump     PathKind = "dump"
    KindRuntime  PathKind = "runtime"
    KindVHD      PathKind = "vhd"      // 虚拟磁盘
    KindSystem   PathKind = "system"   // pagefile/hiberfil/swapfile
    KindUnknown  PathKind = "unknown"
)

type KnownFolder struct {
    ID           string `json:"id"`             // FOLDERID 名，如 "Downloads"
    DisplayName  string `json:"display_name"`   // "下载"
    RawValue     string `json:"raw_value"`      // 注册表原始值（可能含 %USERPROFILE%）
    ActualPath   string `json:"actual_path"`    // 解析后的实际路径
    Exists       bool   `json:"exists"`
    Redirected   bool   `json:"redirected"`     // 是否非默认位置
    OneDriveManaged bool `json:"onedrive_managed"`
    OneDriveReason  string `json:"onedrive_reason,omitempty"`
    Migratable   bool   `json:"migratable"`
    BlockReason  string `json:"block_reason,omitempty"`
    SizeOnDisk   int64  `json:"size_on_disk"`
}
```

### 2.5 Finding（核心产出）

```go
// model/finding.go
type Level string
const (
    LevelNever   Level = "L0"   // 绝对禁止
    LevelConfirm Level = "L1"   // 需确认
    LevelSafe    Level = "L2"   // 可安全清理
    LevelInfo    Level = "L3"   // 仅报告
)

type LevelSubtype string
const (
    SubSystem        LevelSubtype = "system"          // L0
    SubCredential    LevelSubtype = "credential"      // L0
    SubUserGenerated LevelSubtype = "user-generated"  // L0
    SubUserData      LevelSubtype = "user-data"       // L1
    SubEffort        LevelSubtype = "effort"          // L1
)

type Finding struct {
    Path        string       `json:"path"`
    Kind        PathKind     `json:"kind"`
    Level       Level        `json:"level"`
    Subtype     LevelSubtype `json:"subtype,omitempty"`

    OwnerAppID  string `json:"owner_app_id,omitempty"`
    OwnerName   string `json:"owner_name,omitempty"`

    OnDisk      int64 `json:"on_disk"`
    Logical     int64 `json:"logical"`
    FileCount   int64 `json:"file_count"`

    // 可解释性（FR-31，必填）
    Reason        string  `json:"reason"`          // 为什么是这个等级（中文，给用户看）
    Consequence   string  `json:"consequence"`     // 删了会怎样
    Recommendation string `json:"recommendation"`  // 更好的做法
    MatchRule     string  `json:"match_rule"`      // 命中的规则 ID 或 baseline/ heuristic 标识
    Confidence    float64 `json:"confidence"`

    // 状态
    Exists       bool `json:"exists"`
    IsReparse    bool `json:"is_reparse,omitempty"`
    ReparseTag   string `json:"reparse_tag,omitempty"`
    LinkTarget   string `json:"link_target,omitempty"`
    IsOneDrive   bool `json:"is_onedrive,omitempty"`
    OnCloudOnly  bool `json:"on_cloud_only,omitempty"`
    TooRecent    bool `json:"too_recent,omitempty"`
    InUse        bool `json:"in_use,omitempty"`
    OrphanCache  bool `json:"orphan_cache,omitempty"`   // 孤儿缓存（主程序已不存在）

    // 子项提示（用于 L0 目录里"其中 X MB 可清理"）
    SafeSubBytes int64 `json:"safe_sub_bytes,omitempty"`
    Note         string `json:"note,omitempty"`
}
```

### 2.6 迁移候选与系统设置

```go
// model/plan.go
type Mechanism string
const (
    MechJunction     Mechanism = "junction"        // 1
    MechAppConfig    Mechanism = "app-config"      // 2
    MechEnvVar       Mechanism = "env-var"         // 3
    MechKnownFolder  Mechanism = "known-folder"    // 4
    MechRegistry     Mechanism = "registry"        // 5
    MechVHD          Mechanism = "vhd"             // 6
    MechSystemSetting Mechanism = "system-setting" // 7
)

type Risk string
const (
    RiskLow    Risk = "low"
    RiskMedium Risk = "medium"
    RiskHigh   Risk = "high"
)

type MigrationCandidate struct {
    ID          string    `json:"id"`
    AppID       string    `json:"app_id,omitempty"`
    AppName     string    `json:"app_name,omitempty"`
    Source      string    `json:"source"`
    SizeOnDisk  int64     `json:"size_on_disk"`
    Mechanism   Mechanism `json:"mechanism"`
    Target      string    `json:"target,omitempty"`        // 建议目标
    Risk        Risk      `json:"risk"`
    NeedAdmin   bool      `json:"need_admin"`
    NeedStop    []string  `json:"need_stop,omitempty"`     // 需停止的服务/进程
    NeedRestart bool      `json:"need_restart,omitempty"`
    AlreadyMigrated bool  `json:"already_migrated,omitempty"` // 如 JAVA_HOME 已在 D 盘
    Reason      string    `json:"reason"`                  // 为什么用这个机制
    Caveats     []string  `json:"caveats,omitempty"`       // 注意事项
    Precheck    *PrecheckResult `json:"precheck,omitempty"`
    ExtraSavings int64    `json:"extra_savings,omitempty"` // 附加可回收（如 VHDX 收缩）
    ExtraSavingsNote string `json:"extra_savings_note,omitempty"`
}

type SystemTweak struct {
    ID          string `json:"id"`        // "pagefile" / "hiberfil"
    Title       string `json:"title"`     // "将虚拟内存移到 D 盘"
    CurrentPath string `json:"current_path"`
    CurrentSize int64  `json:"current_size"`
    Reclaimable int64  `json:"reclaimable"`
    Command     string `json:"command,omitempty"`   // 展示给用户的等价命令，如 "powercfg /h off"
    NeedAdmin   bool   `json:"need_admin"`
    NeedRestart bool   `json:"need_restart"`
    Reversible  bool   `json:"reversible"`
    RevertHint  string `json:"revert_hint"`
    Effects     []string `json:"effects"`    // 副作用
    Tradeoffs   []TweakOption `json:"tradeoffs,omitempty"`  // 多选项（如休眠的三种选择）
    Reason      string `json:"reason"`
}

type TweakOption struct {
    Label      string `json:"label"`        // "完全关闭休眠"
    Command    string `json:"command"`      // "powercfg /h off"
    Reclaimable int64 `json:"reclaimable"`
    Effect     string `json:"effect"`       // "失去休眠与快速启动"
    Recommended bool  `json:"recommended"`
}
```

### 2.7 扫描统计（含归因缺口）

```go
type ScanStats struct {
    FilesScanned   int64         `json:"files_scanned"`
    DirsScanned    int64         `json:"dirs_scanned"`
    BytesLogical   int64         `json:"bytes_logical"`
    BytesOnDisk    int64         `json:"bytes_on_disk"`
    SkippedDirs    int64         `json:"skipped_dirs"`
    Errors         int64         `json:"errors"`
    ReparsePoints  int64         `json:"reparse_points"`
    HardLinksDeduped int64       `json:"hard_links_deduped"`
    CloudPlaceholders int64      `json:"cloud_placeholders"`
    SkippedReasons []SkippedDir  `json:"skipped_reasons,omitempty"`
    Coverage       CoverageStats `json:"coverage"`
}

// 覆盖率：让用户知道"这份报告有多完整"（对应 FR：标注归因缺口）
type CoverageStats struct {
    AttributedBytes  int64   `json:"attributed_bytes"`    // 已归因
    UnattributedBytes int64  `json:"unattributed_bytes"`  // 未归因
    VolumeUsedBytes  int64   `json:"volume_used_bytes"`   // 卷实际已用（权威值）
    CoveragePercent  float64 `json:"coverage_percent"`    // attributed / volume_used
    GapNote          string  `json:"gap_note,omitempty"`  // "约 16 GB 因权限不足无法归因"
}
```

### 2.8 预算总览（报告头部的「可回收空间总览」）

```go
type ReclaimSummary struct {
    TotalReclaimable  int64 `json:"total_reclaimable"`
    ByMigration       int64 `json:"by_migration"`
    BySystemTweak     int64 `json:"by_system_tweak"`
    ByDeletionSafe    int64 `json:"by_deletion_safe"`       // L2
    ByDeletionConfirm int64 `json:"by_deletion_confirm"`    // L1（不计入 total）
    UserDataNotToTouch int64 `json:"user_data_not_to_touch"` // 用户数据，不提供删除
    Groups []ReclaimGroup `json:"groups"`
}

type ReclaimGroup struct {
    Label     string `json:"label"`       // "迁移到 D 盘"
    Bytes     int64  `json:"bytes"`
    ItemCount int    `json:"item_count"`
    Action    string `json:"action"`      // "migrate" / "setting" / "delete" / "info"
}
```

---

## 3. 变更计划（Plan）

```go
// model/plan.go
const PlanSchemaVersion = "1.0"

type Plan struct {
    SchemaVersion string    `json:"schema_version"`
    ToolVersion   string    `json:"tool_version"`
    CreatedAt     time.Time `json:"created_at"`
    SourceScan    string    `json:"source_scan"`      // 来源于哪份 scan.json（含其时间戳与哈希）
    ScanHash      string    `json:"scan_hash"`        // 防篡改/防过期

    TargetVolume  string    `json:"target_volume"`    // "D:"
    Operators     []Operator `json:"operators"`

    Summary       PlanSummary `json:"summary"`
}

type OperatorType string
const (
    OpDeleteDir      OperatorType = "delete-dir"
    OpDeleteFiles    OperatorType = "delete-files"
    OpRunCommand     OperatorType = "run-command"     // 如 `npm cache clean --force`
    OpMigrateEnv     OperatorType = "migrate-env"
    OpMigrateJunction OperatorType = "migrate-junction"
    OpMigrateAppConfig OperatorType = "migrate-app-config"
    OpMigrateKnownFolder OperatorType = "migrate-known-folder"
    OpMigrateVHD     OperatorType = "migrate-vhd"
    OpChangeSetting  OperatorType = "change-setting"
    OpCompactVHD     OperatorType = "compact-vhd"     // Optimize-VHD
)

type Operator struct {
    ID          string       `json:"id"`
    Type        OperatorType `json:"type"`
    Enabled     bool         `json:"enabled"`        // 用户可在 plan.json 里改 false 跳过
    Description string       `json:"description"`    // 人类可读的单步说明

    // 通用字段
    Path        string `json:"path,omitempty"`
    Target      string `json:"target,omitempty"`
    Pattern     string `json:"pattern,omitempty"`
    Command     string `json:"command,omitempty"`
    Args        []string `json:"args,omitempty"`

    // 机制特有
    EnvVarName  string `json:"env_var_name,omitempty"`
    EnvVarOld   *string `json:"env_var_old,omitempty"`
    EnvVarNew   string `json:"env_var_new,omitempty"`
    RegistryKey string `json:"registry_key,omitempty"`
    RegistryValue string `json:"registry_value,omitempty"`
    SettingName string `json:"setting_name,omitempty"`

    // 安全
    Level       Level  `json:"level,omitempty"`       // 删除类算子的等级
    Risk        Risk   `json:"risk"`
    NeedAdmin   bool   `json:"need_admin"`
    NeedRestart bool   `json:"need_restart"`

    ExpectedBytes int64 `json:"expected_bytes"`       // 预期回收
    Reversible    bool  `json:"reversible"`

    DependsOn   []string `json:"depends_on,omitempty"` // 执行顺序依赖（如先停服务再搬）
    Precheck    *PrecheckResult `json:"precheck,omitempty"`
}

type PlanSummary struct {
    OperatorCount int   `json:"operator_count"`
    DeleteCount   int   `json:"delete_count"`
    MigrateCount  int   `json:"migrate_count"`
    SettingCount  int   `json:"setting_count"`
    TotalReclaim  int64 `json:"total_reclaim_bytes"`
    NeedAdmin     bool  `json:"need_admin"`
    NeedRestart   bool  `json:"need_restart"`
    RiskCounts    map[Risk]int `json:"risk_counts"`
}
```

---

## 4. 变更日志（Journal）

```go
const JournalSchemaVersion = "1.0"

type Journal struct {
    SchemaVersion string          `json:"schema_version"`
    PlanID        string          `json:"plan_id"`
    StartedAt     time.Time       `json:"started_at"`
    FinishedAt    *time.Time      `json:"finished_at,omitempty"`
    DryRun        bool            `json:"dry_run"`
    Entries       []JournalEntry  `json:"entries"`
}

type JournalEntry struct {
    Seq         int          `json:"seq"`
    OperatorID  string       `json:"operator_id"`
    Timestamp   time.Time    `json:"timestamp"`
    Status      string       `json:"status"`      // pending/running/success/failed/skipped/rolledback
    Message     string       `json:"message,omitempty"`
    PrevState   *PrevState   `json:"prev_state,omitempty"`   // 回滚所需
    ActualBytes int64        `json:"actual_bytes,omitempty"` // 实际回收
}

type PrevState struct {
    EnvVarName         *string `json:"env_var_name,omitempty"`
    EnvVarOldValue     *string `json:"env_var_old_value,omitempty"`
    RegistryValueOld   *string `json:"registry_value_old,omitempty"`
    RegistryExisted    *bool   `json:"registry_existed,omitempty"`
    JunctionExisted    bool    `json:"junction_existed"`
    JunctionTarget     string  `json:"junction_target,omitempty"`
    KnownFolderOldPath string  `json:"known_folder_old_path,omitempty"`
    PageFileOld        string  `json:"pagefile_old,omitempty"`      // "C:\pagefile.sys 4096 8192"
    HibernateWasOn     bool    `json:"hibernate_was_on"`
    CommandOutput      string  `json:"command_output,omitempty"`
}
```

---

## 5. CLI 命令树

```
winclean
├── scan                    扫描磁盘，产出 scan.json
│   --disk C[,D]            指定盘（默认所有固定盘）
│   --path DIR              定点扫描某目录（可重复）
│   --deep                  深度模式
│   --max-depth N           快速模式的深度上限（默认 4）
│   --min-size SIZE         只上报 >= SIZE 的条目（默认 10MB）
│   --exclude GLOB          额外排除（可重复）
│   --no-apps               跳过软件清单识别
│   --no-heuristic          跳过启发式路径发现
│   --format table|json     终端展示格式
│   -o, --output FILE       输出 scan.json 路径（默认 ./winclean-scan.json）
│   --workers N             并发数
│   --verify                扫描后做一致性自校验（03 §8）
│   -v, --verbose / -q, --quiet / --no-progress
│
├── report                  从 scan.json 渲染报告
│   -i, --input FILE        默认 ./winclean-scan.json
│   --format table|json|html|md
│   --group-by level|app|size|action
│   --level L0,L1,L2,L3     只显示指定等级
│   --min-size SIZE
│   --top N                 只显示前 N 项
│   --show-skipped          显示被跳过的（占用中/太新/权限不足）
│   -o, --output FILE       html/md 时输出到文件
│
├── apps                   列出软件清单
│   -i, --input FILE
│   --category CAT          按分类过滤
│   --volume C|D            按安装盘过滤
│   --unknown               只看未识别的
│   --orphaned              只看卸载残留
│   --sort size|name|volume
│   --stats                 输出规则库覆盖度统计（unknown 占比）
│
├── app NAME               查看单个软件详情（"这软件是干嘛的"）
│   -i, --input FILE
│   --json
│
├── paths                  列出路径与可迁移项
│   -i, --input FILE
│   --migratable            只看可迁移
│   --kind cache|data|vhd|...
│   --owner APP_ID
│   --min-size SIZE
│
├── dirs                   列出目录占用排行（纯空间视角）
│   -i, --input FILE
│   --top N
│   --min-size SIZE
│   --depth N
│   --volume C
│
├── unknown                列出未归因/未识别的大目录（提示可提交规则）
│   -i, --input FILE
│   --min-size SIZE
│
├── explain PATH           解释某个路径为什么被判成这个等级（可解释性工具）
│   -i, --input FILE
│   ★ 用于调试规则与向用户证明判定的合理性
│
├── plan                   生成变更计划（本期只生成不执行）
│   migrate                生成迁移计划
│     -i, --input FILE
│     --target D:          目标盘
│     --target-root DIR    目标根目录（默认 D:\Migrated）
│     --select IDS         只选指定项（默认按风险分组交互选择）
│     --include-risk low|medium|high
│     --no-precheck        跳过预检（不推荐）
│     -o, --output FILE    默认 ./winclean-plan.json
│   clean                  生成清理计划（仅 L2）
│     --include-l1         包含 L1（需逐项确认）
│     --age-days N         临时文件最小年龄（默认 1 天）
│     --max-total SIZE     总量上限
│   settings               生成系统设置调整计划（页面文件/休眠）
│
├── apply                  执行计划（★ 本期不实现，只保留命令位与 dry-run 输出）
│   -i, --input FILE
│   --dry-run              默认行为
│   --yes                  跳过确认
│   --journal FILE
│
├── undo                   回滚（★ 本期不实现）
│   --journal FILE
│
├── rules                  规则库相关
│   list                   列出所有规则
│   show ID                查看单条规则
│   validate               校验规则库（schema + 冲突检测）
│   coverage               规则库覆盖度报告
│   path                   打印规则库所在目录（便于用户添加自定义规则）
│
├── doctor                 环境自检（版本、权限、WebView2、编码、规则库完整性）
└── version
```

### 5.1 本期实现范围

| 命令 | M1 | M2 | M3 | M4 | M5 | M6 |
| --- | --- | --- | --- | --- | --- | --- |
| `scan` | ✅ 骨架 | +清单 | +路径 | +判定 | — | — |
| `dirs` / `unknown` | ✅ | | | | | |
| `report` | ✅ table | +apps | +paths | +等级 | +html | +总览 |
| `apps` / `app` | | ✅ | | | | |
| `paths` | | | ✅ | | | |
| `explain` | | | | ✅ | | |
| `rules` | | ✅ validate | | | | |
| `plan clean` | | | | | ✅ | |
| `plan migrate` / `plan settings` | | | | | | ✅ |
| `apply` / `undo` | ❌ 不实现（命令存在但只输出 dry-run） | | | | | |

---

## 6. 输出示例

### 6.1 `scan --format table`

```
$ winclean scan --disk C
扫描 C:\ ... 完成 (18.4s, 61.2万文件, 跳过 312 目录)

卷  总容量  已用      剩余      使用率
C:  237 GB  233.1 GB  3.9 GB   98.4%   ⚠️ 空间紧张

【可回收空间总览】预计可回收 ≈ 100.3 GB
  迁移到 D 盘     ≈ 66.2 GB  (12 项)
  调整系统设置     ≈ 34.0 GB  (2 项，需管理员)
  删除（L2 安全）   ≈  2.4 GB  (34 项)

【占用排行 Top 10】
  36.0 GB  docker_data.vhdx        Docker 数据盘        L3  迁移
  24.3 GB  C:\pagefile.sys         虚拟内存             L0  改设置
  22.3 GB  C:\Windows              系统                 L0
  14.2 GB  C:\Program Files        程序                 L0
   9.7 GB  C:\hiberfil.sys         休眠映像             L0  关功能
   9.4 GB  JetBrains (2 版本)       IDE 索引            L1/L2 旧版可清
   9.8 GB  C:\Program Files (x86)  程序                 L0
   8.2 GB  Yarn\Cache              Yarn 包缓存          L1  迁移
   6.9 GB  Tencent                 聊天记录与文件        L0  用户数据
   6.7 GB  Cursor                  IDE 数据             L0/L2 需细分

【归因覆盖率】84.1%  已归因 196.0 GB / 卷已用 233.1 GB
  ⚠️ 约 37 GB 未归因（无管理员权限无法读取回收站/卷影副本）
     以管理员权限运行可获得完整结果

详细结果已写入 ./winclean-scan.json
```

### 6.2 `explain`（可解释性工具）

```
$ winclean explain "C:\Users\33380\AppData\Local\Google\Chrome\User Data"
路径：C:\Users\33380\AppData\Local\Google\Chrome\User Data
等级：L0 (credential)
占用：1.2 GB（其中 222 MB 为可安全清理的子项）
归属：Google Chrome (browser)

判定过程：
  步骤 1 L0 硬阻断检查        → 未命中
  步骤 2 规则库精准匹配        → ✅ 命中 rule:google.chrome.paths[0]
                               声明等级 L0，理由「浏览器用户数据（配置、书签、Cookie、历史）」
  命中即结束（步骤 3-5 不再执行）

子项明细：
  C:\...\User Data\Default\Cache          222 MB   L2  ← 可清理
  C:\...\User Data\Default\Login Data       2 MB   L0  ← 密码库
  C:\...\User Data\Default\Bookmarks        1 MB   L0  ← 书签
  C:\...\User Data\Default\Code Cache      89 MB   L2  ← 可清理
  ...

更好的做法：整体迁移到 D 盘（Junction 机制，风险低）
```

### 6.3 `plan migrate --dry-run`

```
$ winclean plan migrate --target D: --target-root D:\Migrated
读取 ./winclean-scan.json (2026-09-14 11:20:31)

生成迁移计划（预检通过 10/12 项）

  #  大小     项目                     机制            风险  需管理员  需停服务
  1  36.0 GB  docker_data.vhdx         虚拟磁盘迁移      中    ❌       Docker,WSL
  2   8.2 GB  Yarn\Cache               环境变量          低    ❌       —
  3   5.1 GB  Android\Sdk              环境变量          中    ❌       —
  4   3.5 GB  ~\.cache                 Junction         低    ❌       —
  5   3.0 GB  .m2\repository           应用配置          低    ❌       —
  6   2.0 GB  nvm                      环境变量          低    ❌       —
  7   1.3 GB  npm-cache (两处)          环境变量          低    ❌       —
  8   0.3 GB  pip\Cache                环境变量          低    ❌       —
  9   0.3 GB  Temp                     环境变量          中    ❌       —
 10  24.3 GB  pagefile.sys             系统设置          中    ✅       重启
 11   9.7 GB  hiberfil.sys             系统设置          中    ✅       —
 12   6.9 GB  Tencent 数据              应用自身配置       高    —        —
     ⚠️  #12 需在微信/QQ 内手动操作，本工具不代劳

  跳过：
    · Desktop / Pictures  — 已被 OneDrive 接管
    · JAVA_HOME / Maven_HOME — 已迁移，无需处理

合计可回收：≈ 100.3 GB
计划已写入 ./winclean-plan.json
⚠️ 请审阅该文件，可手动将某条改为 "enabled": false 以跳过
⚠️ 执行功能尚未实现（M7），当前仅供审阅
```

### 6.4 `apps --stats`（规则库覆盖度）

```
$ winclean apps --stats
软件条目总数：207（去重后 189）
  按来源：HKLM64 61 / HKLM32 128 / HKCU 18
  被过滤的非软件条目：2（Chrome 扩展）
  卸载残留（Orphaned）：7

规则库覆盖度：
  精确匹配      118  (62.4%)
  模糊匹配       31  (16.4%)
  Publisher 兜底   22  (11.6%)
  未识别         18  ( 9.5%)  ✅ 低于 20% 目标

  未识别 Top 5（建议补充规则）：
    secoresdk            857 MB
    @mmx-agentelectron-updater  729 MB
    ichat                425 MB
    com.adobe.dunamis    221 MB
    lenovo               562 MB
```

---

## 7. 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 成功 |
| 1 | 一般错误 |
| 2 | 参数错误 |
| 3 | 找不到/无法解析 `scan.json` |
| 4 | 规则库校验失败 |
| 5 | 预检失败（存在 blocker） |
| 6 | 部分成功（有跳过项） |
| 7 | 需要管理员权限 |
| 130 | 用户中断（Ctrl+C） |

**约定**：退出码 6（部分成功）在自动化场景很有用——扫描完成了但有目录因权限跳过，调用方可以据此决定是否提权重试。

---

## 8. 与后续 GUI 的接口边界

本期 CLI 的所有能力都必须能通过 `scan.json` + `plan.json` 两个文件被外部消费。这意味着：

- **Wails/Web GUI（M8）只需读这两个 JSON**，可以把 Go 后端当成纯数据源
- `report --format json` 输出的是**结构化的报告模型**（不是 `scan.json` 的镜像），专门为展示优化（已排序、已分组、已算好百分比）
- 进度反馈通过 stderr 的结构化事件（可选 `--progress-format json`），便于 GUI 显示进度条

```go
// report 模型（专为展示优化）
type ReportModel struct {
    GeneratedAt time.Time      `json:"generated_at"`
    Volumes     []VolumeBrief  `json:"volumes"`
    Summary     ReclaimSummary `json:"summary"`
    Groups      []ReportGroup  `json:"groups"`
    Coverage    CoverageStats  `json:"coverage"`
    Warnings    []string       `json:"warnings"`
}

type ReportGroup struct {
    Key         string        `json:"key"`      // "migrate" / "setting" / "L2" / "L1" / "L0"
    Title       string        `json:"title"`    // "迁移到 D 盘"
    TotalBytes  int64         `json:"total_bytes"`
    Items       []ReportItem  `json:"items"`
}

type ReportItem struct {
    Path        string   `json:"path"`
    DisplayName string   `json:"display_name"`
    Bytes       int64    `json:"bytes"`
    Percent     float64  `json:"percent"`       // 占该组总量的百分比
    Level       Level    `json:"level"`
    Kind        PathKind `json:"kind"`
    Owner       string   `json:"owner"`
    Action      string   `json:"action"`        // migrate/setting/delete/info
    Mechanism   Mechanism `json:"mechanism,omitempty"`
    Risk        Risk     `json:"risk,omitempty"`
    Badges      []string `json:"badges,omitempty"`   // ["需管理员","需重启","OneDrive","孤儿缓存"]
    Reason      string   `json:"reason"`
    Consequence string   `json:"consequence"`
    Recommendation string `json:"recommendation"`
}
```
