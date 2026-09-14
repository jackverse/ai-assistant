package deploy

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestDiscoverSynthetic 用合成目录验证扫描规则（确定性，任何机器都能跑）。
func TestDiscoverSynthetic(t *testing.T) {
	root := t.TempDir()

	// 聚合 pom：声明两个后端模块
	writeFileT(t, filepath.Join(root, "demo-proj", "pom.xml"), `<?xml version="1.0"?>
<project>
  <artifactId>demo-parent</artifactId>
  <packaging>pom</packaging>
  <modules>
    <module>demo-admin</module>
    <module>demo-pub</module>
  </modules>
</project>`)
	writeFileT(t, filepath.Join(root, "demo-proj", "demo-admin", "pom.xml"), `<project><artifactId>demo-admin</artifactId></project>`)
	writeFileT(t, filepath.Join(root, "demo-proj", "demo-pub", "pom.xml"), `<project><artifactId>demo-pub</artifactId></project>`)

	// 前端模块：脚本名刻意不统一，且用 vite（产物 dist）
	writeFileT(t, filepath.Join(root, "demo-proj", "demo-admin-web", "package.json"), `{
  "name": "demo-admin-web",
  "scripts": {
    "dev": "vite",
    "demo build:prod": "vite build --mode production",
    "demo build:test": "vite build --mode test"
  }
}`)
	writeFileT(t, filepath.Join(root, "demo-proj", "demo-admin-web", "vite.config.ts"), "export default {}")

	// 没有构建脚本的 package.json：应被标注而不是静默忽略
	writeFileT(t, filepath.Join(root, "demo-proj", "no-script", "package.json"), `{"name":"no-script","scripts":{"lint":"eslint ."}}`)

	// node_modules 里的 package.json 必须被忽略
	writeFileT(t, filepath.Join(root, "demo-proj", "demo-admin-web", "node_modules", "x", "package.json"), `{"name":"x"}`)

	// 独立项目（无 modules 的 pom）
	writeFileT(t, filepath.Join(root, "solo", "pom.xml"), `<project><artifactId>solo</artifactId></project>`)

	projects, notes, err := DiscoverProjects(root, 3)
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	for _, n := range notes {
		t.Logf("提示: %s", n)
	}

	byRoot := map[string]DiscoveredProject{}
	for _, p := range projects {
		byRoot[p.Root] = p
		t.Logf("项目 %s（%s）：%d 个模块  %s", p.Name, p.Root, len(p.Modules), p.Note)
		for _, m := range p.Modules {
			t.Logf("    %-18s %-9s script=%q source=%s output=%s %s",
				m.Name, m.Type, m.Script, m.Source, m.Output, m.Note)
		}
	}

	proj, ok := byRoot["demo-proj"]
	if !ok {
		t.Fatalf("未发现 demo-proj，实际: %v", keys2(byRoot))
	}
	if len(proj.Modules) != 3 {
		t.Errorf("demo-proj 应有 3 个模块（2 后端 + 1 前端），实际 %d", len(proj.Modules))
	}

	var adminWeb *DiscoveredModule
	for i := range proj.Modules {
		if proj.Modules[i].Name == "demo-admin-web" {
			adminWeb = &proj.Modules[i]
		}
	}
	if adminWeb == nil {
		t.Fatal("未发现 demo-admin-web")
	}
	// 关键：脚本名不统一时，必须挑到带 prod 的那个
	if adminWeb.Script != "demo build:prod" {
		t.Errorf("应挑到 \"demo build:prod\"，实际 %q", adminWeb.Script)
	}
	if adminWeb.Source != "dist" {
		t.Errorf("vite 项目产物目录应为 dist，实际 %q", adminWeb.Source)
	}

	// 后端产物命名应与既有工具一致（连字符→下划线）
	if _, ok := byRoot["solo"]; !ok {
		t.Error("独立 pom 应被发现为单模块项目")
	}
	var adminMod DiscoveredModule
	for _, m := range proj.Modules {
		if m.Name == "demo-admin" {
			adminMod = m
		}
	}
	if adminMod.Output != "demo_admin" {
		t.Errorf("后端产物名应为 demo_admin（与既有工具一致），实际 %q", adminMod.Output)
	}

	// node_modules 里的 package.json 不能被发现
	for _, m := range proj.Modules {
		if strings.Contains(m.Root, "node_modules") {
			t.Errorf("不应发现 node_modules 下的模块: %s", m.Root)
		}
	}
	// 没有构建脚本的目录应给出提示
	if !strings.Contains(proj.Note, "no-script") {
		t.Errorf("没有构建脚本的模块应被标注提醒，实际 note=%q", proj.Note)
	}
}

// TestPrecheckBlocksIncompleteConfig 验证预检能拦住不完整的配置。
//
// 这是「傻瓜式」的保障：配置不全时不能让人点了才开始失败。
func TestPrecheckBlocksIncompleteConfig(t *testing.T) {
	// 完全空配置
	res := Precheck(Config{}, RunOptions{})
	if res.OK {
		t.Error("空配置不应通过预检")
	}
	var notOK []string
	for _, it := range res.Items {
		if it.Critical && !it.OK {
			notOK = append(notOK, it.Name)
		}
	}
	t.Logf("未通过项: %v", notOK)
	if len(notOK) == 0 {
		t.Error("应至少有一项关键检查未通过")
	}
	// 每一条未通过都必须给出修复建议——只说失败不说怎么办等于没说
	for _, it := range res.Items {
		if it.Critical && !it.OK && it.Fix == "" {
			t.Errorf("检查项 %q 未通过但没有修复建议", it.Name)
		}
	}
}

// TestPrecheckReady 验证配置齐全时预检通过并列出将产出的产物。
func TestPrecheckReady(t *testing.T) {
	base := t.TempDir()
	outDir := t.TempDir()

	// 一个前端模块（不需要 maven），用 node 直接当脚本可行：这里只做目录与配置检查
	modDir := filepath.Join(base, "proj", "web")
	writeFileT(t, filepath.Join(modDir, "package.json"),
		`{"name":"web","scripts":{"build":"vite build"}}`)

	cfg := Config{
		BasePath: base, OutputDir: outDir, KeepEnv: "prod",
		Projects: []ProjectConfig{{
			Name: "proj", Root: "proj",
			Modules: []ModuleConfig{{Name: "web", Type: "frontend", Script: "build", Source: "dist"}},
		}},
	}
	res := Precheck(cfg, RunOptions{Project: "proj", OutputDir: outDir})
	for _, it := range res.Items {
		t.Logf("%s %-22s %s", map[bool]string{true: "✓", false: "✗"}[it.OK], it.Name, it.Detail)
	}

	if !res.OK {
		t.Fatalf("配置齐全时应通过预检，未通过项见上")
	}
	if len(res.Planned) != 1 {
		t.Fatalf("应列出 1 条将产出的产物，实际 %d", len(res.Planned))
	}
	if !strings.HasSuffix(res.Planned[0].Path, "web.zip") {
		t.Errorf("前端产物应为 web.zip，实际 %s", res.Planned[0].Path)
	}
	if res.UsingJDK == "" {
		t.Error("应记录实际使用的 JDK（让用户知道用的是哪个）")
	}
}

// TestDiscoverRealProjectIfPresent 在真实项目上跑一次扫描（不存在则跳过）。
//
// 合成目录只能验证规则，真实项目才能暴露规则的盲区
// （例如 pom 里的属性占位、多层模块嵌套）。
func TestDiscoverRealProjectIfPresent(t *testing.T) {
	const root = `D:\code`
	if !dirExists(root) {
		t.Skipf("%s 不存在，跳过", root)
	}
	projects, notes, err := DiscoverProjects(root, 3)
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	t.Logf("在 %s 下发现 %d 个项目，%d 条提示", root, len(projects), len(notes))
	for _, p := range projects {
		t.Logf("  %-32s %d 个模块 %s", p.Root, len(p.Modules), truncate(p.Note, 60))
		for _, m := range p.Modules {
			t.Logf("      %-26s %-9s %s", m.Name, m.Type, truncate(m.Script, 40))
		}
	}
	for _, n := range notes {
		t.Logf("  提示: %s", truncate(n, 120))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func keys2(m map[string]DiscoveredProject) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
