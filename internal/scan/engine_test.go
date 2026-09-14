package scan

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"winclean/internal/model"
)

// writeFile 写入指定大小的文件（内容无关，只关心大小）。
func writeFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatalf("写入文件失败: %v", err)
	}
}

// findDir 在结果中查找指定路径的目录条目。
func findDir(res []model.DirEntry, path string) (model.DirEntry, bool) {
	for _, d := range res {
		if strings.EqualFold(d.Path, path) {
			return d, true
		}
	}
	return model.DirEntry{}, false
}

// scanOut 是测试用的扫描产出快照。
type scanOut struct {
	Dirs         []model.DirEntry
	Stats        model.ScanStats
	Warnings     []string
	LargestFiles int
}

// runScan 用测试友好的默认值执行一次扫描。
func runScan(t *testing.T, root string, opts Options) scanOut {
	t.Helper()
	opts.Roots = []string{root}
	if opts.MinSize == 0 {
		opts.MinSize = 1 // 避免被默认的 10MB 门槛挡掉测试用的小文件
	}
	if opts.Workers == 0 {
		opts.Workers = 4
	}
	opts.TopFiles = 10

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	return scanOut{
		Dirs:         res.Dirs,
		Stats:        res.Stats,
		Warnings:     res.Warnings,
		LargestFiles: len(res.LargestFiles),
	}
}

// TestScanTotalsIncludeDeepFiles 验证「上报粒度」与「总量」是两件事。
//
// MaxDepth 限制的是【上报哪些节点】，而根节点的总量必须包含所有后代。
// 如果为了「快」而在 MaxDepth 处停止遍历，父目录体积会偏小，
// 进而让用户误以为系统盘还有空间——这比慢更危险。
func TestScanTotalsIncludeDeepFiles(t *testing.T) {
	root := t.TempDir()

	// 深度 1 放一个大文件，深度 5 再放一个大文件
	writeFile(t, filepath.Join(root, "shallow.bin"), 100_000)
	writeFile(t, filepath.Join(root, "a", "b", "c", "d", "deep.bin"), 200_000)

	res := runScan(t, root, Options{MaxDepth: 1})

	rootEntry, ok := findDir(res.Dirs, root)
	if !ok {
		t.Fatalf("结果中缺少根节点 %s", root)
	}
	if rootEntry.Files != 2 {
		t.Errorf("根节点文件数应为 2（含深层文件），实际 %d", rootEntry.Files)
	}
	// 两个文件的逻辑大小之和必须被计入根节点
	if rootEntry.Logical < 300_000 {
		t.Errorf("根节点逻辑大小应 ≥300000（含深度 5 的文件），实际 %d", rootEntry.Logical)
	}
	// 但深度超过 MaxDepth 的节点不应出现在上报结果里
	if _, found := findDir(res.Dirs, filepath.Join(root, "a", "b", "c", "d")); found {
		t.Error("深度 4 的目录不应出现在 MaxDepth=1 的上报结果中")
	}
}

// TestScanDoesNotFollowJunction 是安全关键测试。
//
// 遍历绝不能跟随重解析点。若跟随，一个指向外部大目录的联接会让统计
// 重复计数；更严重的是后续里程碑基于路径的删除操作会被引导到预期之外。
func TestScanDoesNotFollowJunction(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir() // 与 root 平级，不在 root 之下

	writeFile(t, filepath.Join(root, "inside.bin"), 50_000)
	writeFile(t, filepath.Join(outside, "outside.bin"), 5_000_000)

	link := filepath.Join(root, "link")
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput()
	if err != nil {
		t.Skipf("无法创建 Junction（跳过）：%v，输出：%s", err, strings.TrimSpace(string(out)))
	}

	res := runScan(t, root, Options{MaxDepth: 3})

	rootEntry, ok := findDir(res.Dirs, root)
	if !ok {
		t.Fatalf("结果中缺少根节点")
	}

	// 关键断言：外部那 5 MB 必须完全没有被计入
	if rootEntry.OnDisk >= 5_000_000 {
		t.Errorf("根节点占用 %d 包含了联接目标的内容，说明遍历跟随了重解析点", rootEntry.OnDisk)
	}
	if rootEntry.Files != 1 {
		t.Errorf("根节点文件数应为 1（链接不被展开），实际 %d", rootEntry.Files)
	}

	// 链接本身应被记录为节点，且标注类型与目标
	linkEntry, found := findDir(res.Dirs, link)
	if !found {
		t.Fatalf("链接节点 %s 应出现在结果中", link)
	}
	if !linkEntry.IsReparse {
		t.Error("链接节点应标记 IsReparse")
	}
	if linkEntry.ReparseTag == "" || linkEntry.ReparseTag == "unknown" {
		t.Errorf("链接类型应为 junction，实际 %q", linkEntry.ReparseTag)
	}
	if !strings.EqualFold(strings.TrimRight(linkEntry.LinkTarget, `\`),
		strings.TrimRight(outside, `\`)) {
		t.Errorf("链接目标解析错误\n  期望: %s\n  实际: %s", outside, linkEntry.LinkTarget)
	}

	// 链接之下不应有任何被展开的节点
	for _, d := range res.Dirs {
		if len(d.Path) > len(link) && strings.HasPrefix(strings.ToLower(d.Path), strings.ToLower(link)+`\`) {
			t.Errorf("链接之下的路径 %s 不应被展开", d.Path)
		}
	}
}

// TestScanExcludesDirectory 验证完全跳过的目录不计入任何统计。
func TestScanExcludesDirectory(t *testing.T) {
	root := t.TempDir()
	skip := filepath.Join(root, "skipme")

	writeFile(t, filepath.Join(root, "keep.bin"), 10_000)
	writeFile(t, filepath.Join(skip, "ignored.bin"), 900_000)

	res := runScan(t, root, Options{Excludes: []string{skip}})

	rootEntry, _ := findDir(res.Dirs, root)
	if rootEntry.Files != 1 {
		t.Errorf("排除目录内的文件不应被计入，根节点文件数应为 1，实际 %d", rootEntry.Files)
	}
	if rootEntry.Logical >= 900_000 {
		t.Errorf("排除目录的 900000 字节不应被计入，实际逻辑大小 %d", rootEntry.Logical)
	}
}

// TestScanSummaryOnlyRetainsNodeButNotChildren 验证「只汇总不展开」。
//
// 系统目录（WinSxS、Windows\Installer）体积大且用户有权知道占了多少，
// 因此节点要保留；但逐项细分既慢又无意义，因此子项不保留。
func TestScanSummaryOnlyRetainsNodeButNotChildren(t *testing.T) {
	root := t.TempDir()
	summary := filepath.Join(root, "bigsystem")

	writeFile(t, filepath.Join(summary, "sub", "huge.bin"), 700_000)

	res := runScan(t, root, Options{MaxDepth: 5, SummaryOnly: []string{summary}})

	node, found := findDir(res.Dirs, summary)
	if !found {
		t.Fatalf("只汇总的目录 %s 本身应被保留", summary)
	}
	if !node.SummaryOnly {
		t.Error("该节点应标记 SummaryOnly")
	}
	if node.Logical < 700_000 {
		t.Errorf("只汇总的目录仍需正确统计体积，实际 %d", node.Logical)
	}
	if _, found := findDir(res.Dirs, filepath.Join(summary, "sub")); found {
		t.Error("只汇总目录的子项不应出现在上报结果中")
	}
}

// TestScanEmptyDirectory 验证空目录被正确处理为「一个没有内容的目录」。
//
// 注意：FindFirstFileExW 在空目录上返回 ERROR_FILE_NOT_FOUND，
// 这不是错误，必须被当作空目录处理而不是计入 errors。
//
// 空目录本身不占空间，会被 MinSize 门槛过滤掉，不出现在上报列表里——
// 这是刻意的（报告空目录是噪声）。所以这里断言的是
// 「无错误」+「目录计数包含了它」+「根节点不因此缺失」。
func TestScanEmptyDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	res := runScan(t, root, Options{MaxDepth: 2})

	if res.Stats.Errors != 0 {
		t.Errorf("空目录不应产生错误，实际 errors=%d", res.Stats.Errors)
	}
	// 根 + empty 共 2 个目录，空目录必须被计数，否则说明遍历提前中断了
	if res.Stats.DirsScanned != 2 {
		t.Errorf("目录数应为 2（根 + 空目录），实际 %d", res.Stats.DirsScanned)
	}

	rootEntry, ok := findDir(res.Dirs, root)
	if !ok {
		t.Fatalf("根节点必须出现在结果中")
	}
	if rootEntry.Dirs != 2 {
		t.Errorf("根节点的目录计数应为 2（含空目录），实际 %d", rootEntry.Dirs)
	}
	if rootEntry.Files != 0 {
		t.Errorf("空目录树不应有文件，实际 %d", rootEntry.Files)
	}
}

// TestAlignUp 验证簇对齐计算。
func TestAlignUp(t *testing.T) {
	cases := []struct {
		size, want int64
	}{
		{0, 0},
		{-1, 0},
		{1, 4096},
		{4095, 4096},
		{4096, 4096},
		{4097, 8192},
	}
	for _, c := range cases {
		if got := alignUp(c.size, 4096); got != c.want {
			t.Errorf("alignUp(%d, 4096) = %d，期望 %d", c.size, got, c.want)
		}
	}
	// 簇大小为 0 时应回退到 4096，而不是除零 panic
	if got := alignUp(100, 0); got != 4096 {
		t.Errorf("alignUp(100, 0) 应回退为 4096，实际 %d", got)
	}
}

// TestParseSize 验证大小解析。
func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		err  bool
	}{
		{"10MB", 10 * 1024 * 1024, false},
		{"1GB", 1024 * 1024 * 1024, false},
		{"512KB", 512 * 1024, false},
		{"1.5GB", int64(1.5 * 1024 * 1024 * 1024), false},
		{"1024", 1024, false},
		{"", 0, true},
		{"abc", 0, true},
		{"MB", 0, true},
	}
	for _, c := range cases {
		got, err := ParseSize(c.in)
		if c.err {
			if err == nil {
				t.Errorf("ParseSize(%q) 应报错", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseSize(%q) 意外报错: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseSize(%q) = %d，期望 %d", c.in, got, c.want)
		}
	}
}

// TestJoinChild 验证路径拼接不引入 Clean 带来的副作用。
func TestJoinChild(t *testing.T) {
	cases := []struct{ dir, name, want string }{
		{`C:\`, "Users", `C:\Users`},
		{`C:\Users`, "33380", `C:\Users\33380`},
		{`C:\a\b`, "c", `C:\a\b\c`},
	}
	for _, c := range cases {
		if got := joinChild(c.dir, c.name); got != c.want {
			t.Errorf("joinChild(%q,%q) = %q，期望 %q", c.dir, c.name, got, c.want)
		}
	}
}
