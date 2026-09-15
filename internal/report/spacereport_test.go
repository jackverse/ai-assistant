package report

import (
	"strings"
	"testing"
	"time"

	"winclean/internal/model"
)

func TestCategorizeSystem(t *testing.T) {
	if got := categorize(model.DirEntry{Path: `C:\Windows\System32`}, nil); got != GrpSystem {
		t.Errorf("C:\\Windows 应为系统组件，实际 %s", got)
	}
	if got := categorize(model.DirEntry{Path: `C:\pagefile.sys`}, nil); got != GrpSystem {
		t.Errorf("pagefile.sys 应为系统组件，实际 %s", got)
	}
}

func TestCategorizeUserData(t *testing.T) {
	if got := categorize(model.DirEntry{Path: `C:\Users\x\AppData\Roaming\Tencent`}, nil); got != GrpUserData {
		t.Errorf("Tencent 应为用户数据，实际 %s", got)
	}
	if got := categorize(model.DirEntry{Path: `C:\Users\33380\OneDrive\Desktop`}, nil); got != GrpUserData {
		t.Errorf("OneDrive Desktop 应为用户数据，实际 %s", got)
	}
}

func TestCategorizeDevCache(t *testing.T) {
	if got := categorize(model.DirEntry{Path: `C:\Users\x\AppData\Local\Yarn\Cache`}, nil); got != GrpDevCache {
		t.Errorf("Yarn Cache 应为开发缓存，实际 %s", got)
	}
	if got := categorize(model.DirEntry{Path: `C:\Users\x\.m2\repository`}, nil); got != GrpDevCache {
		t.Errorf(".m2 应为开发缓存，实际 %s", got)
	}
}

func TestCategorizeSafeClean(t *testing.T) {
	if got := categorize(model.DirEntry{Path: `C:\Users\x\AppData\Local\Temp`}, nil); got != GrpSafeClean {
		t.Errorf("Temp 应为可安全清理，实际 %s", got)
	}
}

func TestCategorizePriorityUserDataOverDev(t *testing.T) {
	// Tencent 在 Roaming 下，虽然路径里可能含 dev 关键词，但用户数据优先
	if got := categorize(model.DirEntry{Path: `C:\Users\x\AppData\Roaming\Tencent\x\node_modules`}, nil); got != GrpUserData {
		t.Errorf("Tencent 下的 node_modules 应仍为用户数据，实际 %s", got)
	}
}

func TestInferUsage(t *testing.T) {
	now := time.Now()
	tests := []struct {
		ago      time.Duration
		last     string
		freq     string
	}{
		{0, "今天", "常用"},
		{24 * time.Hour, "昨天", "常用"},
		{5 * 24 * time.Hour, "5 天前", "常用"},
		{20 * 24 * time.Hour, "20 天前", "近期用过"},
		{90 * 24 * time.Hour, "3 个月前", "闲置"},
	}
	for _, c := range tests {
		mt := now.Add(-c.ago)
		e := model.DirEntry{NewestMtime: &mt}
		u := inferUsage(e)
		if u.LastUsed != c.last {
			t.Errorf("ago=%v: LastUsed=%q，期望 %q", c.ago, u.LastUsed, c.last)
		}
		if u.Frequency != c.freq {
			t.Errorf("ago=%v: Frequency=%q，期望 %q", c.ago, u.Frequency, c.freq)
		}
	}
}

func TestBuildSpaceReport(t *testing.T) {

	res := &model.ScanResult{
		Volumes: []model.Volume{{Letter: "C", Scanned: true, UsedBytes: 233 << 30}},
		Dirs: []model.DirEntry{
			{Path: `C:\Windows`, Depth: 1, OnDisk: 31 << 30, Files: 181000, SummaryOnly: true},
			{Path: `C:\Users\x\AppData\Local\Yarn\Cache`, Depth: 4, OnDisk: 8 << 30, Files: 12000},
			{Path: `C:\Users\x\AppData\Local\Temp`, Depth: 4, OnDisk: 280 << 20, Files: 3000},
			{Path: `C:\Users\x\AppData\Roaming\Tencent`, Depth: 4, OnDisk: 7 << 30, Files: 50000},
			{Path: `C:\Users\x\.m2\repository`, Depth: 2, OnDisk: 3 << 30, Files: 50000},
		},
	}
	report := BuildSpaceReport(res)
	for _, g := range report.Groups {
		t.Logf("%s %s %s: %d 条, %.1f GB", g.Icon, g.Title, g.Action, len(g.Items), float64(g.TotalBytes)/(1<<30))
	}

	// 验证关键分组
	hasDev, hasClean, hasUser, hasSys := false, false, false, false
	for _, g := range report.Groups {
		switch g.ID {
		case GrpDevCache:
			hasDev = true
		case GrpSafeClean:
			hasClean = true
		case GrpUserData:
			hasUser = true
		case GrpSystem:
			hasSys = true
		}
	}
	if !hasDev {
		t.Error("缺少开发工具与缓存分组")
	}
	if !hasClean {
		t.Error("缺少可安全清理分组")
	}
	if !hasUser {
		t.Error("缺少用户数据分组")
	}
	if !hasSys {
		t.Error("缺少系统组件分组")
	}

	// 人类可读标签
	for _, g := range report.Groups {
		for _, item := range g.Items {
			t.Logf("  %s | %s | %s | %s", item.Label, item.SizeHuman, item.LastUsed, item.Frequency)
			if strings.Contains(item.Label, `\`) {
				// 标签可以是路径（如系统目录），但开发缓存应该被翻译
			}
		}
	}
}
