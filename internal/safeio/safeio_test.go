package safeio

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"winclean/internal/sys"
	"winclean/internal/winapi"
)

// makeTree 造一棵测试树：文件 + 子目录 + 可选的 Junction。
func makeTree(t *testing.T, root string) {
	t.Helper()
	mk := func(rel, content string) string {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if content == "" {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			return p
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mk("a.txt", "hello")
	mk("sub/b.txt", "world world")
	mk("sub/deep/c.txt", "x")
	mk("empty", "")
}

// TestJunctionRoundTrip 建链接 → 读回目标 → 摘链接，目标内容必须完好。
//
// 这是 docs/design/09 §10 的发布门禁测试
// TestDeleteJunctionDoesNotDeleteTarget 的落地：
// 「删除链接不删目标」是整个工具最危险的单个操作，测试必须锁死。
func TestJunctionRoundTrip(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "real")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "data.txt"), []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(base, "linkdir")
	if err := os.MkdirAll(link, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := winapi.SetJunction(sys.ToExtendedPath(link), target); err != nil {
		t.Fatalf("SetJunction: %v", err)
	}

	// 读回：链接类型与目标正确
	isRe, tag, err := winapi.IsReparsePointRaw(sys.ToExtendedPath(link))
	if err != nil || !isRe || tag != winapi.IO_REPARSE_TAG_MOUNT_POINT {
		t.Fatalf("链接未生效: isRe=%v tag=%x err=%v", isRe, tag, err)
	}
	info, err := winapi.ReadReparsePoint(sys.ToExtendedPath(link))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Target(); got != target {
		t.Fatalf("链接目标不符: got %q want %q", got, target)
	}
	// 链接可穿透读取
	b, err := os.ReadFile(filepath.Join(link, "data.txt"))
	if err != nil || string(b) != "precious" {
		t.Fatalf("经链接读文件失败: %v %q", err, b)
	}

	// 摘链接（安全的删除原语）
	if err := winapi.RemoveDirectoryOnly(sys.ToExtendedPath(link)); err != nil {
		t.Fatalf("RemoveDirectoryOnly: %v", err)
	}
	if _, err := os.Stat(link); !os.IsNotExist(err) {
		t.Fatalf("链接应已消失: %v", err)
	}
	// 目标必须完好
	b, err = os.ReadFile(filepath.Join(target, "data.txt"))
	if err != nil || string(b) != "precious" {
		t.Fatalf("【严重】删除链接影响了目标内容: %v %q", err, b)
	}
}

// TestRemoveTreeRefusesReparse 树内含 Junction 时，RemoveTree 只摘链接、
// 不触碰链接目标的内容。
func TestRemoveTreeRefusesReparse(t *testing.T) {
	base := t.TempDir()
	tree := filepath.Join(base, "tree")
	makeTree(t, tree)

	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tree, "sub", "link")
	if err := os.MkdirAll(link, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := winapi.SetJunction(sys.ToExtendedPath(link), outside); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := RemoveTree(context.Background(), tree, nil); err != nil {
		t.Fatalf("RemoveTree: %v", err)
	}
	if _, err := os.Stat(tree); !os.IsNotExist(err) {
		t.Fatalf("树根应被删除: %v", err)
	}
	// 链接目标必须完好
	b, err := os.ReadFile(filepath.Join(outside, "keep.txt"))
	if err != nil || string(b) != "keep" {
		t.Fatalf("【严重】RemoveTree 触碰了链接目标: %v %q", err, b)
	}
}

// TestCopyTreeVerify 復制 + 校验在含中文/空目录/深层子树的样本上必须一致。
func TestCopyTreeVerify(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "源目录")
	makeTree(t, src)
	if err := os.WriteFile(filepath.Join(src, "sub", "中文 文件.txt"), []byte("内容"), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(base, "dest")
	files, bytes, reparse, err := CopyTree(context.Background(), src, dst, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(reparse) != 0 {
		t.Fatalf("不应有 reparse: %v", reparse)
	}
	srcSum, err := SumTree(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyTrees(srcSum, TreeSummary{Files: files, Bytes: bytes}); err != nil {
		t.Fatalf("校验不一致: %v（源 %+v 复制 %d/%d）", err, srcSum, files, bytes)
	}
	// 内容抽查
	b, err := os.ReadFile(filepath.Join(dst, "sub", "中文 文件.txt"))
	if err != nil || string(b) != "内容" {
		t.Fatalf("复制内容不符: %v %q", err, b)
	}
}

// TestWalkDoesNotDescendReparse 遍历绝不进入 Junction 子树。
func TestWalkDoesNotDescendReparse(t *testing.T) {
	base := t.TempDir()
	tree := filepath.Join(base, "tree")
	makeTree(t, tree)

	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(filepath.Join(outside, "private"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "private", "secret.txt"), []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tree, "linkdir")
	if err := os.MkdirAll(link, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := winapi.SetJunction(sys.ToExtendedPath(link), outside); err != nil {
		t.Fatal(err)
	}

	var seen []string
	res, err := Walk(context.Background(), tree, WalkerConfig{}, func(e Entry) error {
		seen = append(seen, e.Path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range seen {
		if filepath.IsAbs(p) && (p == filepath.Join(link, "private") || p == filepath.Join(link, "private", "secret.txt")) {
			t.Fatalf("遍历进入了链接内部: %s", p)
		}
	}
	if res.ReparseSkipped == 0 {
		t.Fatalf("链接应被计入跳过")
	}
}
