package winapi

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestCompareAllocationAPIs 对比两个「实际磁盘占用」来源是否一致。
//
// 目的：确认 FILE_STANDARD_INFO.AllocationSize（本工具的深度模式所依赖的字段）
// 与 GetCompressedFileSize（Windows 自带的「磁盘上的大小」API）给出相同结果。
// 若两者不一致，说明结构体布局或访问方式有问题——那会导致整个体积统计不可信。
//
// 通过环境变量 WC_TEST_DIR 指定要抽查的目录；未设置时跳过。
func TestCompareAllocationAPIs(t *testing.T) {
	dir := os.Getenv("WC_TEST_DIR")
	if dir == "" {
		t.Skip("未设置 WC_TEST_DIR，跳过")
	}

	type sample struct {
		path       string
		logical    int64
		alloc      int64
		compressed int64
		links      uint32
	}

	var samples []sample
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		ext := `\\?\` + p
		md, err := GetFileMetadata(ext)
		if err != nil {
			return nil
		}
		csize, _ := GetCompressedFileSize(ext)
		samples = append(samples, sample{
			path:       p,
			logical:    info.Size(),
			alloc:      md.Allocation,
			compressed: int64(csize),
			links:      md.Links,
		})
		if len(samples) >= 12 {
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历失败: %v", err)
	}
	if len(samples) == 0 {
		t.Skip("没有取到样本")
	}

	sort.Slice(samples, func(i, j int) bool { return samples[i].logical > samples[j].logical })

	t.Logf("%-14s %-14s %-14s %-6s  %s", "逻辑大小", "AllocationSize", "CompressedSize", "链接数", "文件")
	var mismatches int
	for _, s := range samples {
		flag := ""
		if s.alloc != s.compressed {
			flag = "  ← 不一致"
			mismatches++
		}
		if s.links > 1 {
			flag += "  [硬链接]"
		}
		name := filepath.Base(s.path)
		if len(name) > 40 {
			name = name[:37] + "..."
		}
		t.Logf("%-14d %-14d %-14d %-6d  %s%s",
			s.logical, s.alloc, s.compressed, s.links, name, flag)
	}
	if mismatches > 0 {
		t.Errorf("%d/%d 个样本的两个 API 结果不一致，说明 AllocationSize 读取有误",
			mismatches, len(samples))
	}
}

// TestGetFileMetadataOnKnownFile 验证已知文件的基本字段。
func TestGetFileMetadataOnKnownFile(t *testing.T) {
	windir := os.Getenv("SystemRoot")
	if windir == "" {
		windir = `C:\Windows`
	}
	target := filepath.Join(windir, "System32", "kernel32.dll")
	if _, err := os.Stat(target); err != nil {
		t.Skipf("%s 不存在，跳过", target)
	}

	md, err := GetFileMetadata(`\\?\` + target)
	if err != nil {
		t.Fatalf("GetFileMetadata 失败: %v", err)
	}
	t.Logf("文件=%s", target)
	t.Logf("  ID.VolumeSerial=0x%08X  ID.Index=%d  Links=%d", md.ID.VolumeSerial, md.ID.Index, md.Links)
	t.Logf("  Allocation=%d  EndOfFile=%d", md.Allocation, md.EndOfFile)

	if md.ID.Index == 0 {
		t.Error("文件索引为 0，去重将无法工作")
	}
	if md.Allocation < 0 {
		t.Error("AllocationSize 读取失败")
	}
	if md.Allocation == 0 {
		t.Errorf("Allocation 为 0，但 %s 是一个几百 KB 的文件，读取有误", filepath.Base(target))
	}
	if md.Allocation%512 != 0 {
		t.Errorf("Allocation=%d 不是扇区(512B)的整数倍，可疑", md.Allocation)
	}
	// 正常未压缩文件的分配应不小于逻辑大小（受簇对齐影响会略大）
	if md.Allocation < md.EndOfFile {
		t.Errorf("Allocation(%d) < EndOfFile(%d)，对未压缩文件不应如此",
			md.Allocation, md.EndOfFile)
	}
	if !strings.HasSuffix(strings.ToLower(target), "kernel32.dll") {
		t.Fatalf("测试目标异常")
	}
}
