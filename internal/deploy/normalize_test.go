package deploy

import (
	"os"
	"path/filepath"
	"testing"
)

// TestNormalizeConfigHealsBadRoots 验证配置自愈。
//
// 背景：早期扫描把模块 Root 写成相对【代码根】，拼出的路径会出现
// 重复的项目目录（D:\code\wic-sh\wic-sh\wic-admin）。运行期容错能让
// 预检和打包通过，但配置里一直是错值——界面显示、用户核对都会困惑。
// 自愈负责把配置本身改对。
func TestNormalizeConfigHealsBadRoots(t *testing.T) {
	base := t.TempDir()

	// 真实目录结构：base/wic-sh/{wic-admin,wic-pub,wic-admin-web}
	for _, d := range []string{"wic-admin", "wic-pub", "wic-admin-web"} {
		if err := os.MkdirAll(filepath.Join(base, "wic-sh", d), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &Config{
		BasePath: base,
		Projects: []ProjectConfig{{
			Name: "wic-sh", Root: "wic-sh",
			Modules: []ModuleConfig{
				// 旧格式：root 相对代码根（错误）
				{Name: "wic-admin", Type: "backend", Root: "wic-sh/wic-admin"},
				// 缺省：靠模块名兜（应自愈为具体路径）
				{Name: "wic-pub", Type: "backend"},
				// 前端模块同样被写成相对代码根（错误）
				{Name: "wic-admin-web", Type: "frontend", Root: "wic-sh/wic-admin-web", Script: "build"},
			},
		}},
	}

	changed := NormalizeConfig(cfg, base)
	if !changed {
		t.Fatalf("应检测到需要修正，实际未改写")
	}

	for _, m := range cfg.Projects[0].Modules {
		// 自愈后：主候选（项目根 + Root）必须真实存在
		p := filepath.Join(base, "wic-sh", filepath.FromSlash(m.Root))
		if !dirExists(p) {
			t.Errorf("模块 %s 自愈后 Root=%q，解析为 %s 仍不存在", m.Name, m.Root, p)
		}
		if m.Root == "wic-sh/"+m.Name {
			t.Errorf("模块 %s 的 Root 仍是相对代码根写法: %q", m.Name, m.Root)
		}
	}

	// 幂等：再跑一次不应再产生改写（避免每次启动都写配置）
	if NormalizeConfig(cfg, base) {
		t.Error("第二次规整不应再产生改写（幂等性）")
	}
}

// TestNormalizeConfigSkipsMissingProject 验证项目根不存在时不乱改。
//
// 项目根都找不到时，问题不是「Root 写错」而是「代码被移走/删了」，
// 交给预检明确报错，比这里猜一个路径安全。
func TestNormalizeConfigSkipsMissingProject(t *testing.T) {
	base := t.TempDir()
	cfg := &Config{
		BasePath: base,
		Projects: []ProjectConfig{{
			Name: "gone", Root: "not-here",
			Modules: []ModuleConfig{{Name: "m", Type: "backend", Root: "m"}},
		}},
	}
	if NormalizeConfig(cfg, base) {
		t.Error("项目根不存在时不应改名任何路径")
	}
	if cfg.Projects[0].Modules[0].Root != "m" {
		t.Error("模块 Root 不应被改动")
	}
}

// TestProcAttrDisablesConsoleWindow 验证子进程不弹黑窗。
//
// 本程序是 GUI 子系统，若不显式指定 CREATE_NO_WINDOW，
// 打包时每个 mvn/npm 子进程都会新建一个 cmd 窗口（实测现象）。
func TestProcAttrDisablesConsoleWindow(t *testing.T) {
	attr := procAttr(`chcp 65001 >nul && "C:\a b\npm.cmd" run "x y"`)
	if attr.CreationFlags&createNoWindow == 0 {
		t.Errorf("CreationFlags 必须包含 CREATE_NO_WINDOW(0x%X)，实际 0x%X",
			createNoWindow, attr.CreationFlags)
	}
	if !attr.HideWindow {
		t.Error("HideWindow 应为 true")
	}
	if attr.CmdLine == "" {
		t.Error("CmdLine 不应为空（我们要自己控制引号）")
	}
	if got := attr.CreationFlags & 0x00000008; got != 0 {
		t.Error("不应同时设置 DETACHED_PROCESS（与 CREATE_NO_WINDOW 冲突）")
	}
}
