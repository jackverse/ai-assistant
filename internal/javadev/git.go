package javadev

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Git 变更查看。
//
// 目的很单纯：写代码时最常问的两个问题是「我改了哪些文件」和「这个改动有没有
// 提交」。所以这里只做只读的状态呈现——不做提交、不做切分支、不做拉取推送，
// 那些是有风险的操作，不该藏在一个顺手点开的页签里。
//
// 只读性保证：git status 默认会刷新并回写索引（mtime 缓存），
// 对「只是看一眼」来说这是不必要的写操作，因此设 GIT_OPTIONAL_LOCKS=0，
// 让 git 完全不碰索引文件。

const gitTimeout = 20 * time.Second

// Change 是一个改动文件。
type Change struct {
	Code    string `json:"code"` // 两字符状态码，如 " M" / "??" / "A "
	Path    string `json:"path"`
	Kind    string `json:"kind"` // 中文可读状态
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	Binary  bool   `json:"binary"`
}

// Repo 是一个项目的仓库状态。
type Repo struct {
	Project string   `json:"project"`
	Dir     string   `json:"dir"`
	IsRepo  bool     `json:"is_repo"`
	Branch  string   `json:"branch"`
	Commit  string   `json:"commit"` // 最近一次提交：短哈希 + 标题 + 相对时间
	Changes []Change `json:"changes"`
	// Truncated 为 true 表示改动太多，只展示了前 N 个
	Truncated bool   `json:"truncated"`
	Clean     bool   `json:"clean"`
	Err       string `json:"err"`
}

// maxChanges 单次展示的改动上限。
//
// 一个跑了很久的仓库可能有上千个改动（例如 node_modules 没被 ignore），
// 全铺给界面既没意义也拖慢渲染；超出的数量如实告知用户。
const maxChanges = 400

// GitStatus 查询单个目录的 git 状态。
//
// dir 通常是项目根：项目根往往就是仓库根（本机 wic-sh 即如此）。
// 传入子目录也能用——git 会向上找到仓库，而路径限定在 dir 之内。
func GitStatus(project, dir string) Repo {
	r := Repo{Project: project, Dir: dir}
	if dir == "" {
		r.Err = "项目目录未知"
		return r
	}

	if _, err := exec.LookPath("git"); err != nil {
		r.Err = "没有找到 git 命令（未安装或不在 PATH 里）"
		return r
	}

	// 是否在仓库里：不在仓库时直接给结论，不必再跑后面几条命令
	if out, err := runGit(dir, "rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(out) != "true" {
		r.Err = "这个目录不在 git 仓库里"
		return r
	}
	r.IsRepo = true

	if out, err := runGit(dir, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		r.Branch = strings.TrimSpace(out)
		if r.Branch == "HEAD" {
			r.Branch = "(游离 HEAD)"
		}
	}

	if out, err := runGit(dir, "log", "-1", "--date=relative", "--format=%h\x1f%s\x1f%cd"); err == nil {
		parts := strings.Split(strings.TrimSpace(out), "\x1f")
		if len(parts) == 3 {
			r.Commit = fmt.Sprintf("%s %s（%s）", parts[0], parts[1], parts[2])
		} else if strings.TrimSpace(out) != "" {
			r.Commit = strings.TrimSpace(out)
		}
	} else {
		r.Commit = "（还没有提交）"
	}

	// 行数统计：相对 HEAD 的累加（含已暂存与未暂存），未跟踪文件不在其中
	numstat := map[string][2]int{}
	binary := map[string]bool{}
	if out, err := runGit(dir, "diff", "--numstat", "HEAD"); err == nil {
		parseNumstat(out, numstat, binary)
	}

	out, err := runGit(dir, "status", "--porcelain=v1", "-z", "--", ".")
	if err != nil {
		r.Err = "读取 git 状态失败：" + err.Error()
		return r
	}
	r.Changes = parsePorcelainZ(out, numstat, binary)
	sort.SliceStable(r.Changes, func(i, j int) bool {
		return statusWeight(r.Changes[i].Code) < statusWeight(r.Changes[j].Code)
	})
	if len(r.Changes) > maxChanges {
		r.Changes = r.Changes[:maxChanges]
		r.Truncated = true
	}
	r.Clean = len(r.Changes) == 0
	return r
}

// GitStatusAll 查询多个项目（并发执行：用户点了刷新就该立刻有响应，
// 串行跑几个仓库会明显变慢）。
func GitStatusAll(dirs map[string]string) []Repo {
	type job struct {
		name string
		dir  string
	}
	jobs := make([]job, 0, len(dirs))
	for name, dir := range dirs {
		jobs = append(jobs, job{name, dir})
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].name < jobs[j].name })

	out := make([]Repo, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			out[i] = GitStatus(j.name, j.dir)
		}(i, j)
	}
	wg.Wait()
	return out
}

// runGit 执行一条只读 git 命令。
//
// 三处细节都是必要的：
//   - --no-pager：否则 git 可能挂在分页器上等输入，界面就永远转圈。
//   - core.quotepath=false：否则中文文件名会被转义成 \344\270\255 这种八进制。
//   - GIT_OPTIONAL_LOCKS=0：让 status 不回写索引，保证这一步真的是只读。
func runGit(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()

	full := append([]string{"--no-pager", "-C", dir, "-c", "core.quotepath=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	cmd.Env = gitEnv()

	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			msg := strings.TrimSpace(string(ee.Stderr))
			if msg == "" {
				msg = ee.Error()
			}
			return string(out), fmt.Errorf("%s", msg)
		}
		return string(out), err
	}
	return string(out), nil
}

// gitEnv 让 git 在无终端环境下也不会阻塞等输入。
func gitEnv() []string {
	env := make([]string, 0, 8)
	for _, kv := range childEnv("", "") {
		if strings.HasPrefix(strings.ToUpper(kv), "GIT_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"GIT_TERMINAL_PROMPT=0", // 需要输入时直接失败，不挂在等输入上
		"GIT_OPTIONAL_LOCKS=0",  // 不回写索引：查询保持只读
		"GIT_PAGER=cat",
	)
}

// parseNumstat 解析 `git diff --numstat` 输出：每行「增\t删\t路径」，
// 二进制文件的两个数字是 "-"。
func parseNumstat(out string, dst map[string][2]int, binary map[string]bool) {
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		path := normalizeGitPath(parts[2])
		if parts[0] == "-" || parts[1] == "-" {
			binary[path] = true
			continue
		}
		a, err1 := strconv.Atoi(parts[0])
		d, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil {
			continue
		}
		dst[path] = [2]int{a, d}
	}
}

// parsePorcelainZ 解析 `--porcelain=v1 -z` 输出。
//
// 用 -z（NUL 分隔）而不是按行解析：文件名里可能有空格、引号甚至换行，
// 按行拆会把一个改动拆成两行。重命名/复制条目后面还会紧跟一个原路径字段，
// 必须吃掉，否则会把原路径当成一个「新增文件」显示出来。
func parsePorcelainZ(out string, numstat map[string][2]int, binary map[string]bool) []Change {
	fields := strings.Split(out, "\x00")
	var changes []Change
	for i := 0; i < len(fields); i++ {
		entry := fields[i]
		if len(entry) < 4 {
			continue
		}
		code := entry[:2]
		path := strings.TrimPrefix(entry[3:], "./")

		if code[0] == 'R' || code[0] == 'C' {
			// 下一个字段是原路径；展示用新路径，原路径附在后面更直观
			if i+1 < len(fields) && fields[i+1] != "" {
				orig := strings.TrimPrefix(fields[i+1], "./")
				path = path + "  ← " + orig
				i++
			}
		}

		c := Change{Code: code, Path: path, Kind: describeStatus(code)}
		if n, ok := numstat[strings.TrimPrefix(entry[3:], "./")]; ok {
			c.Added, c.Deleted = n[0], n[1]
		}
		if binary[strings.TrimPrefix(entry[3:], "./")] {
			c.Binary = true
		}
		changes = append(changes, c)
	}
	return changes
}

// describeStatus 把两字符状态码翻成中文。
func describeStatus(code string) string {
	x, y := code[0], code[1]
	switch {
	case x == '?' && y == '?':
		return "未跟踪"
	case x == 'U' || y == 'U' || (x == 'A' && y == 'A') || (x == 'D' && y == 'D'):
		return "冲突"
	}

	// 优先说已暂存（X）那一侧，它更能代表「这次提交会带上什么」
	main := x
	staged := true
	if x == ' ' || x == '?' {
		main = y
		staged = false
	}

	var name string
	switch main {
	case 'M':
		name = "修改"
	case 'A':
		name = "新增"
	case 'D':
		name = "删除"
	case 'R':
		name = "重命名"
	case 'C':
		name = "复制"
	case 'T':
		name = "类型变更"
	default:
		name = "变更"
	}
	if staged {
		return name + "（已暂存）"
	}
	return name
}

// statusWeight 决定列表里的排序：冲突最要紧，其次是已暂存，最后是未跟踪。
func statusWeight(code string) int {
	switch {
	case strings.Contains(code, "U"):
		return 0
	case code[0] != ' ' && code[0] != '?':
		return 1
	case code[1] != ' ':
		return 2
	case code == "??":
		return 3
	}
	return 4
}

func normalizeGitPath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "./")
	if filepath.Separator != '/' {
		p = strings.ReplaceAll(p, "\\", "/")
	}
	return p
}
