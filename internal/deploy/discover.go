package deploy

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 项目自动扫描：从代码根目录推断出「有哪些项目、各有哪些模块」，
// 让用户不必手抄模块清单（这正是既有工具做不到的地方）。

// DiscoveredModule 是扫描发现的一个模块。
type DiscoveredModule struct {
	Name   string `json:"name"`
	Type   string `json:"type"`   // backend | frontend
	Root   string `json:"root"`   // 相对代码根目录
	Script string `json:"script"` // frontend 的 npm 脚本（已按优先级挑选）
	Source string `json:"source"` // frontend 产物目录
	Output string `json:"output"` // 产物名
	Note   string `json:"note,omitempty"`
}

// DiscoveredProject 是扫描发现的一个项目。
type DiscoveredProject struct {
	Name    string             `json:"name"`
	Root    string             `json:"root"` // 相对代码根目录
	Modules []DiscoveredModule `json:"modules"`
	Note    string             `json:"note,omitempty"`
	// LastModified 是项目根 pom.xml 的修改时间（Unix 秒）。
	// 用于按「最近动过的排前面」排序——代码总目录下动辄上百个项目，
	// 全铺出来是噪音；实测 D:\code 下有 137 个项目、单个项目 78 个模块，
	// 不排序用户根本找不到自己要打的那个。
	LastModified int64 `json:"last_modified"`
	// BackendCount / FrontendCount 便于列表页一眼看出项目构成
	BackendCount  int `json:"backend_count"`
	FrontendCount int `json:"frontend_count"`
}

// 遍历时跳过的目录。这些目录里几乎不可能有项目根，
// 但会大幅拖慢扫描（target/ 与 node_modules/ 尤其大）。
var discoverSkipDirs = map[string]bool{
	"target": true, "node_modules": true, ".git": true, ".idea": true,
	"out": true, "dist": true, "build": true, ".mvn": true, ".gradle": true,
	".vscode": true, "logs": true, "log": true, ".settings": true,
}

// pomInfo 是从 pom.xml 里取到的关键信息。
type pomInfo struct {
	ArtifactID string
	Packaging  string
	Modules    []string
}

type pomXML struct {
	ArtifactID string `xml:"artifactId"`
	Packaging  string `xml:"packaging"`
	Modules    struct {
		Module []string `xml:"module"`
	} `xml:"modules"`
}

type packageJSON struct {
	Name    string            `json:"name"`
	Scripts map[string]string `json:"scripts"`
}

// DiscoverProjects 扫描代码根目录，推断项目与模块。
//
// 返回的项目与提示都会展示给用户确认，而不是直接生效——
// 扫描是启发式的，用户必须有机会纠正（见设计文档 16 §4）。
func DiscoverProjects(codeRoot string, maxDepth int) ([]DiscoveredProject, []string, error) {
	codeRoot = strings.TrimSpace(codeRoot)
	if codeRoot == "" {
		return nil, nil, fmt.Errorf("代码根目录为空")
	}
	if !dirExists(codeRoot) {
		return nil, nil, fmt.Errorf("代码根目录不存在: %s", codeRoot)
	}
	if maxDepth <= 0 {
		maxDepth = 3
	}

	var notes []string

	// ── 1. 找所有 pom.xml ──────────────────────────────────
	poms := map[string]pomInfo{} // key: 绝对目录
	walkErr := filepath.WalkDir(codeRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 权限不足等直接跳过，不中断整次扫描
		}
		if d.IsDir() {
			base := strings.ToLower(d.Name())
			if discoverSkipDirs[base] {
				return fs.SkipDir
			}
			if rel, rerr := filepath.Rel(codeRoot, p); rerr == nil && rel != "." {
				if strings.Count(rel, string(os.PathSeparator)) >= maxDepth {
					return fs.SkipDir
				}
			}
			return nil
		}
		if !strings.EqualFold(d.Name(), "pom.xml") {
			return nil
		}
		dir := filepath.Dir(p)
		info, perr := parsePom(p)
		if perr != nil {
			notes = append(notes, fmt.Sprintf("无法解析 %s：%v", p, perr))
			// 仍然当作一个后端模块，避免整项目丢失
			info = pomInfo{}
		}
		poms[dir] = info
		return nil
	})
	if walkErr != nil {
		return nil, notes, walkErr
	}
	if len(poms) == 0 {
		notes = append(notes, "未在代码根目录下找到 pom.xml（Maven 项目）或 package.json（前端项目）")
	}

	// ── 2. 判定项目根 ─────────────────────────────────────
	// 含 <modules> 的 pom 是聚合模块，即项目根
	projectRoots := map[string]bool{}
	for dir, info := range poms {
		if len(info.Modules) > 0 {
			projectRoots[dir] = true
		}
	}
	// 孤立的 pom（无 modules，且不在任何项目根之下）=> 自己就是一个单模块项目
	for dir := range poms {
		if projectRoots[dir] {
			continue
		}
		inside := false
		for root := range projectRoots {
			if isUnderDir(dir, root) {
				inside = true
				break
			}
		}
		if !inside {
			projectRoots[dir] = true
		}
	}

	var projects []DiscoveredProject
	for root := range projectRoots {
		relRoot, _ := filepath.Rel(codeRoot, root)
		proj := DiscoveredProject{
			Name: filepath.Base(root),
			Root: filepath.ToSlash(relRoot),
		}

		// 后端模块：聚合 pom 的 <modules>；或单模块项目自身
		info := poms[root]
		if len(info.Modules) > 0 {
			for _, m := range info.Modules {
				modDir := filepath.Join(root, filepath.FromSlash(m))
				if !dirExists(modDir) {
					proj.Note = appendNote(proj.Note,
						fmt.Sprintf("模块目录不存在，已跳过：%s", m))
					continue
				}
				proj.Modules = append(proj.Modules, DiscoveredModule{
					Name:   filepath.Base(modDir),
					Type:   "backend",
					// Root 的语义是【相对项目根】——不是相对代码根。
					// 写成相对代码根会让下游拼出 wic-sh/wic-sh/wic-admin 这种重复路径
					// （实测预检就是这样抓到的）。
					Root:   filepath.ToSlash(mustRel(root, modDir)),
					Output: sanitizeBackendOutput(filepath.Base(modDir)),
				})
			}
		} else {
			// 单模块项目：模块目录就是项目根自身
			proj.Modules = append(proj.Modules, DiscoveredModule{
				Name:   filepath.Base(root),
				Type:   "backend",
				Root:   ".",
				Output: sanitizeBackendOutput(filepath.Base(root)),
			})
		}

		// 前端模块：项目根下 ≤2 层找 package.json
		proj.Modules = append(proj.Modules, discoverFrontends(root, &proj)...)

		// 统计与时间戳（供列表排序与筛选）
		for _, m := range proj.Modules {
			if m.Type == "frontend" {
				proj.FrontendCount++
			} else {
				proj.BackendCount++
			}
		}
		if st, err := os.Stat(filepath.Join(root, "pom.xml")); err == nil {
			proj.LastModified = st.ModTime().Unix()
		} else if st, err := os.Stat(filepath.Join(root, "package.json")); err == nil {
			proj.LastModified = st.ModTime().Unix()
		}

		projects = append(projects, proj)
	}

	// 按最近修改倒序：用户最可能打的是刚动过的项目
	sort.Slice(projects, func(i, j int) bool {
		if projects[i].LastModified != projects[j].LastModified {
			return projects[i].LastModified > projects[j].LastModified
		}
		return projects[i].Root < projects[j].Root
	})
	return projects, notes, nil
}

// discoverFrontends 在项目根下找前端模块。
//
// 模块 Root 一律输出为【相对项目根】的路径。
func discoverFrontends(projectRoot string, proj *DiscoveredProject) []DiscoveredModule {
	var out []DiscoveredModule

	_ = filepath.WalkDir(projectRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			base := strings.ToLower(d.Name())
			if discoverSkipDirs[base] {
				return fs.SkipDir
			}
			if p != projectRoot {
				if rel, rerr := filepath.Rel(projectRoot, p); rerr == nil {
					if strings.Count(rel, string(os.PathSeparator)) >= 2 {
						return fs.SkipDir
					}
				}
			}
			return nil
		}
		if !strings.EqualFold(d.Name(), "package.json") {
			return nil
		}

		dir := filepath.Dir(p)
		pkg, perr := parsePackageJSON(p)
		if perr != nil {
			proj.Note = appendNote(proj.Note,
				fmt.Sprintf("%s 的 package.json 无法解析，已跳过", filepath.Base(dir)))
			return nil
		}
		script, score := pickBuildScript(pkg.Scripts)
		if script == "" {
			proj.Note = appendNote(proj.Note,
				fmt.Sprintf("%s 的 package.json 里没有找到构建脚本，需要手动指定", filepath.Base(dir)))
			return nil
		}
		source, srcNote := inferSourceDir(dir, pkg.Scripts[script])
		mod := DiscoveredModule{
			Name:   filepath.Base(dir),
			Type:   "frontend",
			Root:   filepath.ToSlash(mustRel(projectRoot, dir)),
			Script: script,
			Source: source,
			Output: filepath.Base(dir),
		}
		if srcNote != "" {
			mod.Note = srcNote
		}
		if score < 3 {
			mod.Note = appendNote(mod.Note, "构建脚本按模式匹配挑选，请确认是否是你想要的那个")
		}
		out = append(out, mod)
		return nil
	})

	return out
}

// pickBuildScript 从 scripts 里挑最像"生产构建"的那个。
//
// 为什么不能写死成 "build"：本机实测两个前端模块的脚本名分别叫
// "admin build:prod" 与 "wic-startup-build-prod"，命名毫无规律，
// 只能按模式打分挑选。
//
// 评分：含 prod 且含 build = 4；含 build 且不是 test/dev = 3；含 build = 2；其余 0。
func pickBuildScript(scripts map[string]string) (string, int) {
	best, bestScore := "", 0
	for name := range scripts {
		l := strings.ToLower(name)
		if !strings.Contains(l, "build") {
			continue
		}
		score := 2
		if strings.Contains(l, "prod") {
			score = 4
		} else if !strings.Contains(l, "test") && !strings.Contains(l, "dev") {
			score = 3
		}
		cmp := score > bestScore || (score == bestScore && name < best)
		if cmp {
			best, bestScore = name, score
		}
	}
	return best, bestScore
}

// inferSourceDir 推断前端构建产物目录。
func inferSourceDir(modDir, scriptValue string) (string, string) {
	// 1. 脚本里显式指定了 --outDir
	if i := strings.Index(scriptValue, "--outDir"); i >= 0 {
		rest := strings.TrimSpace(scriptValue[i+len("--outDir"):])
		if f := strings.Fields(rest); len(f) > 0 {
			return strings.Trim(f[0], `"'`), ""
		}
	}
	// 2. Vite 项目默认 dist（vite.config.* 存在即可确认）
	if matches, _ := filepath.Glob(filepath.Join(modDir, "vite.config.*")); len(matches) > 0 {
		return "dist", ""
	}
	// 3. 其它构建工具：给默认值并提醒
	if matches, _ := filepath.Glob(filepath.Join(modDir, "webpack.config.*")); len(matches) > 0 {
		return "dist", "webpack 项目，产物目录按 dist 推测，请确认"
	}
	return "dist", "未能识别构建工具，产物目录按 dist 推测，请确认"
}

// sanitizeBackendOutput 统一后端产物命名。
//
// 与既有工具保持一致：模块 wic-admin 产出 wic_admin_war_exploded.zip
// （连字符换成下划线）。部署脚本可能已经按这个名字取产物，不能随意改。
func sanitizeBackendOutput(name string) string {
	return strings.ReplaceAll(name, "-", "_")
}

func parsePom(path string) (pomInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return pomInfo{}, err
	}
	var p pomXML
	if err := xml.Unmarshal(data, &p); err != nil {
		return pomInfo{}, err
	}
	return pomInfo{
		ArtifactID: strings.TrimSpace(p.ArtifactID),
		Packaging:  strings.TrimSpace(p.Packaging),
		Modules:    cleanList(p.Modules.Module),
	}, nil
}

func parsePackageJSON(path string) (packageJSON, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return packageJSON{}, err
	}
	var pkg packageJSON
	if err := json.Unmarshal(data, &pkg); err != nil {
		return packageJSON{}, err
	}
	return pkg, nil
}

func cleanList(in []string) []string {
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" && !strings.HasPrefix(s, "${") { // 跳过 Maven 属性占位
			out = append(out, s)
		}
	}
	return out
}

func mustRel(base, target string) string {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return target
	}
	return rel
}

func isUnderDir(child, parent string) bool {
	c := strings.ToLower(filepath.Clean(child))
	p := strings.ToLower(filepath.Clean(parent))
	return strings.HasPrefix(c, p+string(os.PathSeparator))
}

func appendNote(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "；" + add
}
