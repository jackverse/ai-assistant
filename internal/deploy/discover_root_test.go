package deploy

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDiscoverModuleRootIsProjectRelative 锁定模块 Root 的语义。
//
// 这是实测踩过的坑：早期版本把模块 Root 写成【相对代码根】，
// 下游拼接后得到 D:\code\wic-sh\wic-sh\wic-admin（项目目录重复一次），
// 预检因此报「模块目录不存在」。语义必须是【相对项目根】。
func TestDiscoverModuleRootIsProjectRelative(t *testing.T) {
	root := t.TempDir()
	// D:\...\<tmp>\myproj\{pom.xml, myproj-admin/pom.xml, myproj-web/package.json}
	writeFileT(t, filepath.Join(root, "myproj", "pom.xml"), `<?xml version="1.0"?>
<project><artifactId>myproj-parent</artifactId><packaging>pom</packaging>
  <modules><module>myproj-admin</module></modules></project>`)
	writeFileT(t, filepath.Join(root, "myproj", "myproj-admin", "pom.xml"),
		`<project><artifactId>myproj-admin</artifactId></project>`)
	writeFileT(t, filepath.Join(root, "myproj", "myproj-web", "package.json"),
		`{"name":"myproj-web","scripts":{"build:prod":"vite build"}}`)
	writeFileT(t, filepath.Join(root, "myproj", "myproj-web", "vite.config.ts"), "export default {}")

	projects, _, err := DiscoverProjects(root, 3)
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("应发现 1 个项目，实际 %d", len(projects))
	}
	p := projects[0]
	if p.Root != "myproj" {
		t.Errorf("项目 Root 应为 myproj，实际 %q", p.Root)
	}

	for _, m := range p.Modules {
		if m.Root == "" {
			t.Fatalf("模块 %s 的 Root 为空", m.Name)
		}
		// 关键断言：模块 Root 必须能在【项目根】下解析到真实目录
		got := filepath.Join(root, filepath.FromSlash(p.Root), filepath.FromSlash(m.Root))
		if !dirExists(got) {
			t.Errorf("模块 %s 的 Root=%q 相对项目根解析为 %s，该目录不存在",
				m.Name, m.Root, got)
		}
		// 反向断言：不能再拼一次项目根（那是重复路径 bug 的特征）
		dup := filepath.Join(root, filepath.FromSlash(p.Root), filepath.FromSlash(p.Root))
		if dirExists(dup) && got == dup {
			t.Errorf("模块 %s 的 Root 是相对代码根写的（会拼出重复路径）", m.Name)
		}
	}
}

// TestResolveModuleDirToleratesLegacyRoot 验证对历史错误配置的容错。
//
// 用户配置里可能已经写入了「相对代码根」的 root（旧版本扫描产生），
// 解析时必须能自动找回正确目录，否则用户只能靠手改配置自救。
func TestResolveModuleDirToleratesLegacyRoot(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "wic-sh", "wic-admin")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}

	proj := ProjectConfig{Name: "wic-sh", Root: "wic-sh"}

	// 正确写法：root 相对项目根
	if got := ResolveModuleDir(base, proj, ModuleConfig{Name: "wic-admin", Root: "wic-admin"}); got != real {
		t.Errorf("标准配置应解析到 %s，实际 %s", real, got)
	}
	// 缺省 root：用模块名
	if got := ResolveModuleDir(base, proj, ModuleConfig{Name: "wic-admin"}); got != real {
		t.Errorf("缺省 root 应解析到 %s，实际 %s", real, got)
	}
	// 旧版错误写法：root 相对代码根（wic-sh/wic-admin）
	if got := ResolveModuleDir(base, proj, ModuleConfig{Name: "wic-admin", Root: "wic-sh/wic-admin"}); got != real {
		t.Errorf("旧版错误的 root 应被自动纠正为 %s，实际 %s", real, got)
	}
	// 单模块项目：root 为 "."
	if got := ResolveModuleDir(base, ProjectConfig{Name: "wic-sh", Root: "wic-sh"}, ModuleConfig{Name: "wic-sh", Root: "."}); got != filepath.Join(base, "wic-sh") {
		t.Errorf("root=. 应解析到项目根，实际 %s", got)
	}
}
