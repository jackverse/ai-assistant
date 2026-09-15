package javadev

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("本机没有 git，跳过")
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v 失败: %v\n%s", args, err, out)
	}
}

// 造一个真仓库：改动、暂存、未跟踪三种状态都要能正确归类。
func TestGitStatusClassifiesChanges(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "tracked.txt")
	gitRun(t, dir, "commit", "-q", "-m", "init")

	// 1) 已修改（未暂存） 2) 新增并暂存 3) 未跟踪
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("v1\nv2\nv3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "staged.txt")
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("?\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := GitStatus("demo", dir)
	if !r.IsRepo {
		t.Fatalf("没认出这是 git 仓库：%s", r.Err)
	}
	if r.Branch == "" || r.Branch == "(游离 HEAD)" {
		t.Errorf("分支识别异常: %q", r.Branch)
	}
	if !strings.Contains(r.Commit, "init") {
		t.Errorf("最近提交应含标题 init，实际 %q", r.Commit)
	}
	if r.Clean {
		t.Error("有改动时不该标记为 clean")
	}

	byPath := map[string]Change{}
	for _, c := range r.Changes {
		byPath[c.Path] = c
	}
	if c, ok := byPath["tracked.txt"]; !ok {
		t.Errorf("没有列出 tracked.txt：%+v", r.Changes)
	} else {
		if c.Kind != "修改" {
			t.Errorf("tracked.txt 的状态 = %q，想要 修改", c.Kind)
		}
		if c.Added != 2 {
			t.Errorf("tracked.txt 新增行数 = %d，想要 2", c.Added)
		}
	}
	if c, ok := byPath["staged.txt"]; !ok {
		t.Errorf("没有列出 staged.txt：%+v", r.Changes)
	} else if !strings.Contains(c.Kind, "新增") {
		t.Errorf("staged.txt 的状态 = %q，想要 新增（已暂存）", c.Kind)
	}
	if c, ok := byPath["untracked.txt"]; !ok {
		t.Errorf("没有列出 untracked.txt：%+v", r.Changes)
	} else if c.Kind != "未跟踪" {
		t.Errorf("untracked.txt 的状态 = %q，想要 未跟踪", c.Kind)
	}
}

// 文件名带空格时必须原样显示——按行解析的实现在这里会露馅。
func TestGitStatusHandlesSpacesInFilenames(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "my file.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := GitStatus("demo", dir)
	found := false
	for _, c := range r.Changes {
		if c.Path == "my file.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("带空格的文件名没有正确解析：%+v", r.Changes)
	}
}

func TestGitStatusOutsideRepo(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	r := GitStatus("demo", dir)
	if r.IsRepo {
		t.Fatalf("临时目录不该被当成仓库：%+v", r)
	}
	if r.Err == "" {
		t.Error("不在仓库里时要给出原因，而不是一块空白")
	}
	if r.Clean {
		t.Error("无法判断时不应声称「没有改动」")
	}
}

// 用本机真实仓库核对一次：这次改造动了不少文件，状态里应该看得到。
func TestGitStatusAgainstCurrentRepo(t *testing.T) {
	requireGit(t)
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	r := GitStatus("winclean", root)
	if !r.IsRepo {
		t.Skipf("%s 不是 git 仓库：%s", root, r.Err)
	}
	if r.Truncated {
		t.Logf("改动较多，已截断展示（共 %d 条）", len(r.Changes))
	}
	t.Logf("分支=%s 改动=%d 最近提交=%s", r.Branch, len(r.Changes), r.Commit)
}
