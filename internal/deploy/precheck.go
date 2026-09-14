package deploy

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// 预检：把「跑了两分钟才失败」变成「点之前就知道能不能开始」。
//
// 每一项都要给出「不通过时怎么修」——只说"失败"而不说"怎么办"，
// 对使用者没有任何帮助。

// CheckItem 是一条预检结果。
type CheckItem struct {
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Detail   string `json:"detail"`
	Fix      string `json:"fix,omitempty"`      // 不通过时的修复建议
	Critical bool   `json:"critical"`           // 关键项：不通过则禁止打包
}

// PlannedOutput 是一条「本次将产出」。
type PlannedOutput struct {
	Project string `json:"project"`
	Module  string `json:"module"`
	Type    string `json:"type"`
	Path    string `json:"path"`
}

// PrecheckResult 是完整的预检结果。
type PrecheckResult struct {
	Items   []CheckItem     `json:"items"`
	Planned []PlannedOutput `json:"planned"`
	OK      bool            `json:"ok"` // 全部关键项通过
	Notes   []string        `json:"notes"`
	// 实际将使用的工具链（展示给用户，证明"用的是哪一个"）
	UsingJDK   string `json:"using_jdk"`
	UsingMaven string `json:"using_maven"`
}

// Precheck 检查一次打包能否开始。
func Precheck(cfg Config, opts RunOptions) PrecheckResult {
	var res PrecheckResult

	// 解析出本次要打的模块
	type job struct {
		proj ProjectConfig
		mod  ModuleConfig
	}
	var jobs []job
	for _, p := range cfg.Projects {
		if opts.Project != "" && p.Name != opts.Project {
			continue
		}
		want := map[string]bool{}
		for _, m := range opts.Modules {
			want[m] = true
		}
		for _, m := range p.Modules {
			if len(want) > 0 && !want[m.Name] {
				continue
			}
			jobs = append(jobs, job{proj: p, mod: m})
		}
	}

	var (
		needMaven, needNpm bool
	)
	hasFrontend := false
	for _, j := range jobs {
		t := j.mod.Type
		if t == "" {
			t = "backend"
		}
		if t == "frontend" {
			hasFrontend = true
			needNpm = true
		} else {
			needMaven = true
		}
	}

	// ① 代码根目录
	base := strings.TrimSpace(cfg.BasePath)
	switch {
	case base == "":
		addCheck(&res, "代码根目录", false, "未配置",
			"到「配置」页第 1 步选择代码根目录", true)
	case !dirExists(base):
		addCheck(&res, "代码根目录", false, "目录不存在: "+base,
			"到「配置」页第 1 步重新选择", true)
	default:
		addCheck(&res, "代码根目录", true, base, "", true)
	}

	// ② 有模块可打
	if len(jobs) == 0 {
		addCheck(&res, "待打包模块", false, "没有选中任何模块",
			"到「配置」页第 2 步勾选要打包的模块", true)
	} else {
		nFe := 0
		for _, j := range jobs {
			if j.mod.Type == "frontend" {
				nFe++
			}
		}
		addCheck(&res, "待打包模块", true,
			fmt.Sprintf("%d 个（后端 %d / 前端 %d）", len(jobs), len(jobs)-nFe, nFe),
			"", true)
	}

	// ③ JDK
	jdk := strings.TrimSpace(cfg.JDKPath)
	jdkSource := "配置指定"
	if jdk == "" {
		d := Detect(nil)
		if len(d.JDKs) > 0 && d.JDKs[0].Valid {
			jdk = d.JDKs[0].Path
			jdkSource = "自动探测（" + d.JDKs[0].Source + "）"
		}
	}
	if jdk == "" {
		addCheck(&res, "JDK", false, "未找到可用的 JDK",
			"安装 JDK（建议 17）后在「配置」页第 3 步选择；只有 java 没有 javac 的 JRE 不算", true)
	} else if !isFile(filepath.Join(jdk, "bin", "javac.exe")) {
		addCheck(&res, "JDK", false, "指定的目录不是有效 JDK（缺 bin\\javac.exe）: "+jdk,
			"在「配置」页第 3 步改选一个有效 JDK", true)
	} else {
		res.UsingJDK = jdk
		addCheck(&res, "JDK", true, readJDKVersion(jdk)+" · "+jdk+"（"+jdkSource+"）", "", true)
	}

	// ④ Maven（仅后端需要）
	if needMaven {
		mvn := strings.TrimSpace(cfg.MavenPath)
		mvnSource := "配置指定"
		if mvn == "" {
			d := Detect(nil)
			if len(d.Mavens) > 0 && d.Mavens[0].Valid {
				mvn = d.Mavens[0].Path
				mvnSource = "自动探测（" + d.Mavens[0].Source + "）"
			}
		}
		if mvn == "" {
			addCheck(&res, "Maven", false, "有后端模块，但未找到 Maven",
				"安装 Maven 后在「配置」页第 3 步选择 mvn.cmd", true)
		} else if !fileExistsAny(mvn) {
			addCheck(&res, "Maven", false, "指定的 mvn 不存在: "+mvn,
				"在「配置」页第 3 步改选一个有效 Maven", true)
		} else {
			res.UsingMaven = mvn
			addCheck(&res, "Maven", true, mvn+"（"+mvnSource+"）", "", true)
		}
	}

	// ⑤ npm（仅前端需要）
	if needNpm {
		npm := ""
		if p, err := exec.LookPath("npm.cmd"); err == nil {
			npm = p
		} else if p, err := exec.LookPath("npm"); err == nil {
			npm = p
		}
		if npm == "" {
			addCheck(&res, "npm", false, "有前端模块，但 PATH 里没有 npm",
				"安装 Node.js 后重启本程序（PATH 才会刷新）", true)
		} else {
			addCheck(&res, "npm", true, npm, "", true)
		}
	}

	// ⑥ 输出目录
	outDir := strings.TrimSpace(opts.OutputDir)
	if outDir == "" {
		outDir = strings.TrimSpace(cfg.OutputDir)
	}
	switch {
	case outDir == "":
		addCheck(&res, "产物输出目录", false, "未选择",
			"到「配置」页第 3 步选择输出目录（打包页上方也能直接改）", true)
	case !dirExists(outDir):
		// 不存在可以创建，先试一下能不能建
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			addCheck(&res, "产物输出目录", false,
				"目录不存在且无法创建: "+outDir, "换一个可写的位置", true)
		} else {
			addCheck(&res, "产物输出目录", true, outDir+"（已创建）", "", true)
		}
	default:
		if err := checkWritable(outDir); err != nil {
			addCheck(&res, "产物输出目录", false, "目录不可写: "+err.Error(),
				"换一个有写权限的位置", true)
		} else {
			addCheck(&res, "产物输出目录", true, outDir, "", true)
		}
	}

	// ⑦ 逐模块检查目录与配置
	for _, j := range jobs {
		mod := j.mod
		t := mod.Type
		if t == "" {
			t = "backend"
		}
		root := mod.Root
		if root == "" {
			root = mod.Name
		}
		modDir := filepath.Join(base, j.proj.Root, filepath.FromSlash(root))
		label := j.proj.Name + "/" + mod.Name

		if !dirExists(modDir) {
			addCheck(&res, label, false, "模块目录不存在: "+modDir,
				"检查配置里的模块 root 是否正确", true)
			continue
		}

		switch t {
		case "backend":
			if !isFile(filepath.Join(modDir, "pom.xml")) {
				addCheck(&res, label, false, "目录下没有 pom.xml: "+modDir,
					"后端模块必须指向含 pom.xml 的目录", true)
				continue
			}
		case "frontend":
			if !isFile(filepath.Join(modDir, "package.json")) {
				addCheck(&res, label, false, "目录下没有 package.json: "+modDir,
					"前端模块必须指向含 package.json 的目录", true)
				continue
			}
			if strings.TrimSpace(mod.Script) == "" {
				addCheck(&res, label, false, "未配置构建脚本（npm script）",
					"到「配置」页第 2 步为该模块选择构建脚本", true)
				continue
			}
		default:
			addCheck(&res, label, false, "不支持的模块类型: "+t,
				"类型只能是 backend 或 frontend", true)
			continue
		}

		output := mod.Output
		if output == "" {
			output = mod.Name
		}
		zipName := output + ".zip"
		if t == "backend" {
			zipName = sanitizeBackendOutput(output) + "_war_exploded.zip"
		}
		res.Planned = append(res.Planned, PlannedOutput{
			Project: j.proj.Name, Module: mod.Name, Type: t,
			Path: filepath.Join(outDir, zipName),
		})
		addCheck(&res, label, true, t+" · 产物 "+zipName, "", true)
	}

	// 汇总
	res.OK = true
	for _, it := range res.Items {
		if it.Critical && !it.OK {
			res.OK = false
		}
	}
	if !res.OK {
		res.Notes = append(res.Notes, "有未通过的关键检查项，请先按提示修复")
	} else if hasFrontend && res.UsingMaven == "" {
		res.Notes = append(res.Notes, "本次只打前端模块，不需要 Maven")
	}
	return res
}

// checkWritable 判断目录是否可写（不留下垃圾文件）。
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".wc-write-test-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return nil
}

func addCheck(res *PrecheckResult, name string, ok bool, detail, fix string, critical bool) {
	res.Items = append(res.Items, CheckItem{
		Name: name, OK: ok, Detail: detail, Fix: fix, Critical: critical,
	})
}
