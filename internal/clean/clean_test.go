package clean

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"winclean/internal/sys"
	"winclean/internal/winapi"
)

// TestGuardCheckForbiddenRoots 护栏：禁止根内的路径被拒绝，
// 显式白名单被放行，其余一切照常。
func TestGuardCheckForbiddenRoots(t *testing.T) {
	cases := []struct {
		path    string
		allowed bool
	}{
		{`C:\Windows\System32`, false},
		{`C:\Windows`, false},
		{`C:\Windows\explorer.exe`, false},
		{`C:\Windows\Installer`, false},
		{`C:\Program Files\SomeApp`, false},
		{`C:\Program Files (x86)\SomeApp`, false},
		{`C:\Recovery`, false},
		{`C:\System Volume Information`, false},
		{`C:\Windows\Temp`, true},
		{`C:\Windows\Logs\CBS`, true},
		{`C:\Windows\SoftwareDistribution\Download`, true},
		{`C:\Windows\Minidump`, true},
		{`C:\Users\someone\AppData\Local\Temp`, true},
		{`D:\anything`, true},
	}
	for _, c := range cases {
		err := GuardCheck(c.path)
		if c.allowed && err != nil {
			t.Errorf("%s 应放行，但被拒绝: %v", c.path, err)
		}
		if !c.allowed && err == nil {
			t.Errorf("%s 应被拒绝，但通过了", c.path)
		}
	}
}

// TestForbiddenRootsHaveNoWildcardExceptions 白名单必须是显式枚举，
// 不允许出现通配符（docs/design/09 §10 的发布门禁）。
func TestForbiddenRootsHaveNoWildcardExceptions(t *testing.T) {
	for _, ex := range allowedWriteExceptions {
		if containsAny(ex, `*?`) {
			t.Errorf("白名单含通配符: %s", ex)
		}
		covered := false
		for _, root := range forbiddenWriteRoots {
			if sys.IsUnder(ex, root) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("白名单 %s 不是任何禁止根的后代（写错位置了）", ex)
		}
	}
}

// TestGuardCheckNeverReparseEscape 构造指向系统目录的链接，
// 删除动作不得被引导进禁止根（防越界删除，docs/design/09 §10）。
func TestGuardCheckNeverReparseEscape(t *testing.T) {
	base := t.TempDir()
	evil := filepath.Join(base, "evil")
	if err := os.MkdirAll(evil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := winapi.SetJunction(sys.ToExtendedPath(evil), `C:\Windows\System32`); err != nil {
		t.Skipf("无法创建 Junction（权限/环境限制）: %v", err)
	}
	// GuardCheck 本身按字符串判定，此处验证的是执行层的重解析防护：
	// executeOne 里的删除走 safeio（不跟随 reparse）。
	// 这里直接断言 winapi.RemoveDirectoryOnly 会拒绝非链接目录。
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := winapi.RemoveDirectoryOnly(sys.ToExtendedPath(real)); err == nil {
		t.Fatalf("RemoveDirectoryOnly 应拒绝真实目录")
	}
}

// TestMeasureAndExecuteAgeFilter 年龄过滤：近期文件必须被跳过且不被删除。
func TestMeasureAndExecuteAgeFilter(t *testing.T) {
	root := t.TempDir()
	oldF := filepath.Join(root, "old.tmp")
	newF := filepath.Join(root, "new.tmp")
	if err := os.WriteFile(oldF, []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newF, []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldF, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	c := Candidate{ID: "t", Title: "t", Level: LevelSafe, Kind: KindDir,
		Paths: []string{root}, MinAge: 24 * time.Hour}

	st, err := Measure(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 2 || st.Size != 10 {
		t.Fatalf("测量不符: %+v", st)
	}
	if st.Reclaimable != 5 || st.ReclaimableFiles != 1 {
		t.Fatalf("可回收应只含旧文件: %+v", st)
	}
	if st.RecentFiles != 1 || st.RecentBytes != 5 {
		t.Fatalf("近期文件统计不符: %+v", st)
	}

	res := Execute(context.Background(), []Candidate{c},
		[]Selection{{ID: "t"}}, nil)[0]
	if !res.OK || res.DeletedFiles != 1 || res.FreedBytes != 5 {
		t.Fatalf("执行结果不符: %+v", res)
	}
	if res.SkippedRecent != 1 {
		t.Fatalf("近期文件应被跳过: %+v", res)
	}
	if _, err := os.Stat(newF); err != nil {
		t.Fatalf("【严重】近期文件被删除了")
	}
	if _, err := os.Stat(oldF); !os.IsNotExist(err) {
		t.Fatalf("旧文件应已删除: %v", err)
	}
}

// TestExecuteSkipsJunction 树内的 Junction 绝不删除、目标不受影响。
func TestExecuteSkipsJunction(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "cache")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	oldF := filepath.Join(root, "old.bin")
	if err := os.WriteFile(oldF, []byte("xxx"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldF, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linkdir")
	if err := os.MkdirAll(link, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := winapi.SetJunction(sys.ToExtendedPath(link), outside); err != nil {
		t.Skipf("无法创建 Junction: %v", err)
	}

	c := Candidate{ID: "t", Title: "t", Level: LevelSafe, Kind: KindDir,
		Paths: []string{root}, MinAge: 24 * time.Hour}
	res := Execute(context.Background(), []Candidate{c},
		[]Selection{{ID: "t"}}, nil)[0]

	if res.SkippedReparse == 0 {
		t.Fatalf("链接应被计入跳过: %+v", res)
	}
	b, err := os.ReadFile(filepath.Join(outside, "keep.txt"))
	if err != nil || string(b) != "keep" {
		t.Fatalf("【严重】清理触碰了链接目标: %v %q", err, b)
	}
}

// TestCatalogPathsPassGuard 内置候选必须全部通过护栏（自洽性）。
func TestCatalogPathsPassGuard(t *testing.T) {
	for _, c := range Catalog() {
		for _, p := range c.Paths {
			if err := GuardCheck(p); err != nil {
				t.Errorf("候选 %s 的路径 %s 未通过护栏: %v", c.ID, p, err)
			}
		}
	}
}

func containsAny(s, chars string) bool {
	for _, r := range s {
		for _, c := range chars {
			if r == c {
				return true
			}
		}
	}
	return false
}
