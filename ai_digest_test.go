package main

import (
	"strings"
	"testing"
	"time"

	"winclean/internal/model"
)

// TestBuildScanDigest 验证给大模型的扫描摘要：内容齐全、体积可控、无噪声。
func TestBuildScanDigest(t *testing.T) {
	mt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	res := &model.ScanResult{
		Volumes: []model.Volume{{Letter: "C", IsSystem: true, Scanned: true,
			TotalBytes: 254 << 30, FreeBytes: 4 << 30}},
		Dirs: []model.DirEntry{
			{Path: `C:\Users\x\AppData`, OnDisk: 84 << 30, Files: 800000, NewestMtime: &mt},
			{Path: `C:\Windows`, OnDisk: 31 << 30, Files: 181000, SummaryOnly: true},
			{Path: `C:\链接`, IsReparse: true}, // 对分析无信息量，应被排除
		},
		LargestFiles: []model.LargestFile{
			{Path: `C:\a.vhdx`, OnDisk: 36 << 30, Mtime: &mt},
			{Path: `C:\pagefile.sys`, OnDisk: 24 << 30},
		},
		Warnings: []string{"有权限缺口"},
	}
	d := buildScanDigest(res, map[string]bool{`C:\a.vhdx`: true})
	t.Logf("digest %d 字节:\n%s", len(d), d)

	for _, want := range []string{
		`"letter":"C"`, `84.00`, `31.00`,
		`"newest":"2026-09-01"`, `仅汇总`, `"in_use":true`,
		`"kind":"虚拟磁盘"`, `"warnings"`,
	} {
		if !strings.Contains(d, want) {
			t.Errorf("digest 缺少 %q", want)
		}
	}
	if strings.Contains(d, `C:\链接`) {
		t.Error("重解析点不应进入 digest（不占空间，无分析价值）")
	}

	// 体积预算：目录条目很多时摘要也不能失控（给模型留上下文）
	big := &model.ScanResult{}
	for i := 0; i < 200; i++ {
		big.Dirs = append(big.Dirs, model.DirEntry{Path: `C:\x`, OnDisk: int64(i)})
	}
	if n := len(buildScanDigest(big, nil)); n > 48*1024 {
		t.Errorf("digest 超出体积预算: %d 字节", n)
	}
}
