package migrate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"winclean/internal/sys"
	"winclean/internal/winapi"
)

// setupSource 造一个含文件与内部 Junction 的源目录，返回 (源, 外部链接目标)。
func setupSource(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	src := filepath.Join(base, "cache-src")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("world!!"), 0o644); err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "linkfile.txt"), []byte("linked"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(src, "linkdir")
	if err := os.MkdirAll(link, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := winapi.SetJunction(sys.ToExtendedPath(link), outside); err != nil {
		t.Skipf("无法创建 Junction: %v", err)
	}
	return src, outside
}

func isJunctionTo(t *testing.T, path, want string) bool {
	t.Helper()
	isRe, tag, err := winapi.IsReparsePointRaw(sys.ToExtendedPath(path))
	if err != nil || !isRe || tag != winapi.IO_REPARSE_TAG_MOUNT_POINT {
		return false
	}
	info, err := winapi.ReadReparsePoint(sys.ToExtendedPath(path))
	return err == nil && info.Target() == want
}

// TestMigrateFullCycle 完整生命周期：迁移 → 验证 → 撤销 → 再迁移 → 删备份。
func TestMigrateFullCycle(t *testing.T) {
	src, outside := setupSource(t)
	base := t.TempDir()
	cand := Cand{ID: "test", Name: "测试缓存", Path: src, Size: 20, Risk: RiskLow}
	opts := Options{Base: base, AllowSameVolume: true}

	// ── 第一次迁移 ──
	results := Run(context.Background(), []Cand{cand}, []string{"test"}, opts, nil)
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("迁移失败: %+v", results)
	}
	r := results[0]

	// 原位置是指向目标的 Junction
	if !isJunctionTo(t, src, r.Target) {
		t.Fatalf("原位置应为指向 %s 的 Junction", r.Target)
	}
	// 经原路径可读出内容（软件无感知）
	b, err := os.ReadFile(filepath.Join(src, "a.txt"))
	if err != nil || string(b) != "hello" {
		t.Fatalf("经链接读文件失败: %v %q", err, b)
	}
	// 目标与备份都存在
	if _, err := os.Stat(filepath.Join(r.Target, "sub", "b.txt")); err != nil {
		t.Fatalf("目标数据不完整: %v", err)
	}
	if _, err := os.Stat(r.Backup); err != nil {
		t.Fatalf("备份应存在: %v", err)
	}
	// 内部 Junction 在目标处被重建
	if !isJunctionTo(t, filepath.Join(r.Target, "linkdir"), outside) {
		t.Fatalf("目标内的子目录链接应被重建")
	}

	// ── 撤销 ──
	journals := ListJournals()
	if len(journals) == 0 {
		t.Fatal("journal 应存在")
	}
	if err := Undo(journals[0].ID); err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	if isRe, _, _ := winapi.IsReparsePointRaw(sys.ToExtendedPath(src)); isRe {
		t.Fatalf("撤销后原位置不应再是链接")
	}
	if _, err := os.Stat(filepath.Join(src, "sub", "b.txt")); err != nil {
		t.Fatalf("撤销后原数据应完好: %v", err)
	}
	if _, err := os.Stat(r.Backup); !os.IsNotExist(err) {
		t.Fatalf("撤销后备份应已被改回原名: %v", err)
	}

	// 清理撤销后残留的目标目录，为二次迁移让路（目标绝不覆盖）
	if err := os.RemoveAll(r.Target); err != nil {
		t.Fatal(err)
	}

	// ── 第二次迁移 + 删除备份（延迟清理）──
	results = Run(context.Background(), []Cand{cand}, []string{"test"}, opts, nil)
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("二次迁移失败: %+v", results)
	}
	r = results[0]

	journals = ListJournals()
	if err := PurgeBackup(journals[0].ID); err != nil {
		t.Fatalf("删除备份失败: %v", err)
	}
	if _, err := os.Stat(r.Backup); !os.IsNotExist(err) {
		t.Fatalf("备份应已删除: %v", err)
	}
	// 链接仍然工作，数据在新位置完好
	b, err = os.ReadFile(filepath.Join(src, "a.txt"))
	if err != nil || string(b) != "hello" {
		t.Fatalf("删除备份后链接失效: %v %q", err, b)
	}
}

// TestRunRefusesOverwrite 目标已存在时必须拒绝，绝不覆盖。
func TestRunRefusesOverwrite(t *testing.T) {
	src, _ := setupSource(t)
	base := t.TempDir()
	// 预先创建同名目标
	if err := os.MkdirAll(filepath.Join(base, filepath.Base(src)), 0o755); err != nil {
		t.Fatal(err)
	}
	cand := Cand{ID: "test", Name: "测试", Path: src, Size: 10}
	results := Run(context.Background(), []Cand{cand}, []string{"test"},
		Options{Base: base, AllowSameVolume: true}, nil)
	if len(results) != 1 || results[0].OK {
		t.Fatalf("目标已存在时应拒绝: %+v", results)
	}
	// 源必须完好且未被替换
	if isRe, _, _ := winapi.IsReparsePointRaw(sys.ToExtendedPath(src)); isRe {
		t.Fatalf("【严重】失败的迁移改动了源")
	}
	if _, err := os.Stat(filepath.Join(src, "a.txt")); err != nil {
		t.Fatalf("源数据受损: %v", err)
	}
}

// TestRunIdempotent 已迁移过（原位置已是链接）的候选应跳过而非报错或重复搬。
func TestRunIdempotent(t *testing.T) {
	src, _ := setupSource(t)
	base := t.TempDir()
	cand := Cand{ID: "test", Name: "测试", Path: src, Size: 10}
	opts := Options{Base: base, AllowSameVolume: true}

	results := Run(context.Background(), []Cand{cand}, []string{"test"}, opts, nil)
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("首次迁移失败: %+v", results)
	}

	// 再跑一次（源已是链接）→ 跳过；注意此时目标已存在，
	// 若没有幂等检查会在预检「目标已存在」处失败。
	results = Run(context.Background(), []Cand{cand}, []string{"test"}, opts, nil)
	if len(results) != 1 || !results[0].OK || !results[0].Skipped {
		t.Fatalf("已迁移项应被幂等跳过: %+v", results)
	}
}

// TestJournalSurvivesReopen journal 落盘后能被重新读出（崩溃恢复的基础）。
func TestJournalSurvivesReopen(t *testing.T) {
	j, _, err := NewJournal("test-" + t.Name())
	if err != nil {
		t.Fatal(err)
	}
	rec := ItemRecord{ID: "x", Source: `C:\s`, Target: `D:\t`, Backup: `C:\s.bak`,
		Bytes: 5, Files: 1, Done: true, EnvVar: "FOO", EnvOld: "old", EnvOldSet: true}
	if err := j.Write(Entry{Kind: "item_done", Item: rec}); err != nil {
		t.Fatal(err)
	}
	j.Close()

	recs := ListJournals()
	var found *FileRecord
	for i := range recs {
		if recs[i].ID == "test-"+t.Name() {
			found = &recs[i]
		}
	}
	if found == nil || len(found.Items) != 1 || !found.Items[0].Done || found.Items[0].EnvOld != "old" {
		t.Fatalf("journal 读回不符: %+v", found)
	}
	// 清理测试 journal
	os.Remove(found.Path)
}
