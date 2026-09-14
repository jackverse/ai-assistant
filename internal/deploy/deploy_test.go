package deploy

import (
	"archive/zip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// execLookPathNpm 供测试判断 npm 是否可用。
func execLookPathNpm() (string, error) {
	if p, err := exec.LookPath("npm.cmd"); err == nil {
		return p, nil
	}
	return exec.LookPath("npm")
}

// writeFileT 写文件（自动建目录）。
func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// zipEntries 返回 zip 内的条目名集合。
func zipEntries(t *testing.T, zipPath string) map[string]bool {
	t.Helper()
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("打开 zip 失败: %v", err)
	}
	defer zr.Close()
	out := map[string]bool{}
	for _, f := range zr.File {
		out[f.Name] = true
	}
	return out
}

// TestRunFrontendPackEndToEnd 端到端验证前端打包链路。
//
// 用真实 npm 跑一个最小脚本（只创建 dist 目录），验证：
//   - 配置解析与模块定位
//   - 外部命令调用（cmd.exe 包装、含空格的 script 名引号处理）
//   - 产物目录校验
//   - 压缩与输出路径
//
// 不依赖 maven、不依赖任何真实项目，因此在任何装了 Node 的机器上都能跑。
func TestRunFrontendPackEndToEnd(t *testing.T) {
	if _, err := execLookPathNpm(); err != nil {
		t.Skipf("未找到 npm，跳过：%v", err)
	}

	base := t.TempDir()
	outDir := t.TempDir()

	// 模拟一个前端模块：script 名刻意带空格与冒号，复现 wic-admin-web 的真实命名
	modDir := filepath.Join(base, "demo-proj", "demo-web")
	writeFileT(t, filepath.Join(modDir, "package.json"), `{
  "name": "demo-web",
  "version": "1.0.0",
  "scripts": {
    "demo build:prod": "node -e \"const fs=require('fs');fs.mkdirSync('dist/assets',{recursive:true});fs.writeFileSync('dist/index.html','<html>ok</html>');fs.writeFileSync('dist/assets/app.js','console.log(1)');\""
  }
}`)

	cfg := Config{
		BasePath:  base,
		OutputDir: outDir,
		KeepEnv:   "prod",
		Projects: []ProjectConfig{{
			Name: "demo-proj", Root: "demo-proj",
			Modules: []ModuleConfig{{
				Name: "demo-web", Type: "frontend",
				Script: "demo build:prod", Source: "dist", Output: "demo-web",
			}},
		}},
	}

	ctx := context.Background()
	var (
		sawCmd bool
		done   bool
		allLog []string
	)
	err := Run(ctx, cfg, RunOptions{OutputDir: outDir}, func(ev Event) {
		if ev.Line != "" {
			allLog = append(allLog, ev.Line)
		}
		if strings.Contains(ev.Line, "npm") && strings.Contains(ev.Line, "run") {
			sawCmd = true
		}
		if ev.Kind == "done" {
			done = true
		}
	})
	for _, l := range allLog {
		t.Logf("  | %s", l)
	}
	if err != nil {
		t.Fatalf("打包失败: %v", err)
	}
	if !sawCmd {
		t.Error("日志里没有看到 npm run 命令，说明命令没有真正执行")
	}
	if !done {
		t.Error("没有收到 done 事件")
	}

	zipPath := filepath.Join(outDir, "demo-web.zip")
	entries := zipEntries(t, zipPath)
	if !entries["index.html"] {
		t.Errorf("产物里缺少 index.html，实际条目: %v", keys(entries))
	}
	if !entries["assets/app.js"] {
		t.Errorf("产物里缺少 assets/app.js（子目录未被打包），实际条目: %v", keys(entries))
	}
}

// TestFilterEnvFilesOnlyTouchesAssembly 验证环境过滤的边界。
//
// 这是本模块最关键的安全约束：只删「组装目录」里的配置文件，
// 绝不动源码目录。误删源码里的 application-prod.yml 是不可接受的。
func TestFilterEnvFilesOnlyTouchesAssembly(t *testing.T) {
	assembly := t.TempDir()
	classes := filepath.Join(assembly, "WEB-INF", "classes")
	for _, name := range []string{
		"application.yml",
		"application-prod.yml",
		"application-common-prod.yml",
		"application-database-prod.yml",
		"application-dev.yml",
		"application-test.yml",
		"application-common-dev.yml",
		"application-demo.yml",
		"mapper.xml", // 非配置文件不能被误删
	} {
		writeFileT(t, filepath.Join(classes, name), "x")
	}

	removed, err := filterEnvFiles(assembly, "prod")
	if err != nil {
		t.Fatalf("过滤失败: %v", err)
	}
	if removed != 4 {
		t.Errorf("应移除 4 个非 prod 配置，实际 %d", removed)
	}

	// 保留的必须在
	for _, keep := range []string{
		"application.yml", "application-prod.yml",
		"application-common-prod.yml", "application-database-prod.yml", "mapper.xml",
	} {
		if !isFile(filepath.Join(classes, keep)) {
			t.Errorf("不应删除 %s", keep)
		}
	}
	// 移除的必须不在
	for _, gone := range []string{
		"application-dev.yml", "application-test.yml",
		"application-common-dev.yml", "application-demo.yml",
	} {
		if isFile(filepath.Join(classes, gone)) {
			t.Errorf("应删除 %s", gone)
		}
	}
}

// TestFilterEnvFilesMissingClasses 验证没有 WEB-INF/classes 时不报错（前端产物解压场景）。
func TestFilterEnvFilesMissingClasses(t *testing.T) {
	dir := t.TempDir()
	n, err := filterEnvFiles(dir, "prod")
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if n != 0 {
		t.Errorf("应移除 0 个，实际 %d", n)
	}
}

// TestPrepareAssemblyPrefersWar 验证有 war 时优先解压 war。
func TestPrepareAssemblyPrefersWar(t *testing.T) {
	modDir := t.TempDir()
	target := filepath.Join(modDir, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}

	// 造一个最小 war：WEB-INF/classes/application-prod.yml
	warPath := filepath.Join(target, "demo.war")
	makeZip(t, warPath, map[string]string{
		"WEB-INF/classes/application-prod.yml": "prod",
		"WEB-INF/lib/x.jar":                    "jar",
	})

	assembly, cleanup, err := prepareAssembly(modDir, "demo", func(Event) {})
	if err != nil {
		t.Fatalf("准备组装目录失败: %v", err)
	}
	defer cleanup()

	if !isFile(filepath.Join(assembly, "WEB-INF", "lib", "x.jar")) {
		t.Error("war 里的 lib 没有被解出来")
	}
	if !isFile(filepath.Join(assembly, "WEB-INF", "classes", "application-prod.yml")) {
		t.Error("war 里的 classes 没有被解出来")
	}
}

// TestPrepareAssemblyFallsBackToClasses 验证没有 war 时退化为 target/classes 并告警。
func TestPrepareAssemblyFallsBackToClasses(t *testing.T) {
	modDir := t.TempDir()
	writeFileT(t, filepath.Join(modDir, "target", "classes", "App.class"), "x")

	var warned bool
	assembly, cleanup, err := prepareAssembly(modDir, "demo", func(ev Event) {
		if strings.Contains(ev.Line, "未找到 war") {
			warned = true
		}
	})
	if err != nil {
		t.Fatalf("应退化为 classes 而不是报错: %v", err)
	}
	defer cleanup()

	if !isFile(filepath.Join(assembly, "WEB-INF", "classes", "App.class")) {
		t.Error("classes 没有被复制到 WEB-INF/classes")
	}
	if !warned {
		t.Error("缺少 war 时必须告警（否则用户不知道 lib 可能不全）")
	}
}

// TestPrepareAssemblyNoArtifact 验证没有产物时给出可读错误。
func TestPrepareAssemblyNoArtifact(t *testing.T) {
	modDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(modDir, "target"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, err := prepareAssembly(modDir, "demo", func(Event) {})
	if err == nil {
		t.Fatal("没有产物时应当报错")
	}
	if !strings.Contains(err.Error(), "构建是否成功") {
		t.Errorf("错误信息应给出排查方向，实际: %v", err)
	}
}

// TestQuoteIfNeeded 验证命令行参数的引号处理。
//
// 这是真实踩过的点：npm script 名可以含空格（本机 wic-admin-web 的脚本就叫
// "admin build:prod"），不加引号会被拆成两个参数导致 npm 报错。
func TestQuoteIfNeeded(t *testing.T) {
	cases := []struct{ in, want string }{
		{"simple", "simple"},
		{"admin build:prod", `"admin build:prod"`},
		{"", `""`},
		{`C:\Program Files\npm.cmd`, `"C:\Program Files\npm.cmd"`},
	}
	for _, c := range cases {
		if got := quoteIfNeeded(c.in); got != c.want {
			t.Errorf("quoteIfNeeded(%q) = %s，期望 %s", c.in, got, c.want)
		}
	}
}

// TestImportExternalConfig 验证能把你现有的 bundle-packer 配置转过来。
func TestImportExternalConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// 与真实文件同构（注意 yaml 里的反斜杠要转义）
	writeFileT(t, path, `
output_dir: "D:\\record\\product"
base_path: "D:\\code"
maven_path: "D:\\workplace\\apache-maven-3.9.6\\bin\\mvn.cmd"
jdk_path: "D:\\workplace\\jdk17"

projects:
  - name: "wic-sh"
    root: "wic-sh"
    modules:
      - name: "wic-admin"
      - name: "wic-pub"
      - name: "wic-admin-web"
        type: frontend
        source: "dist"
        script: "admin build:prod"
        output: "wic-admin-web"
`)
	cfg, err := ImportExternalConfig(path)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if cfg.BasePath != `D:\code` || cfg.OutputDir != `D:\record\product` {
		t.Errorf("基础路径解析错误: %+v", cfg)
	}
	if len(cfg.Projects) != 1 || len(cfg.Projects[0].Modules) != 3 {
		t.Fatalf("项目/模块数不对: %+v", cfg.Projects)
	}
	// 缺省 type 应补为 backend
	if cfg.Projects[0].Modules[0].Type != "backend" {
		t.Errorf("缺省 type 应为 backend，实际 %s", cfg.Projects[0].Modules[0].Type)
	}
	// 前端模块的 script/source/output 必须原样保留（尤其含空格的 script）
	fe := cfg.Projects[0].Modules[2]
	if fe.Type != "frontend" || fe.Script != "admin build:prod" ||
		fe.Source != "dist" || fe.Output != "wic-admin-web" {
		t.Errorf("前端模块字段丢失: %+v", fe)
	}
	if cfg.KeepEnv != "prod" {
		t.Errorf("缺省保留环境应为 prod，实际 %q", cfg.KeepEnv)
	}
	t.Logf("导入结果: %s", cfg.Describe())
}

// ── 测试辅助 ──

func makeZip(t *testing.T, zipPath string, files map[string]string) {
	t.Helper()
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
