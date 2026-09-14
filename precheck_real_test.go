package main

import (
	"os"
	"testing"

	"winclean/internal/config"
	"winclean/internal/deploy"
)

// TestPrecheckWithRealConfig 用本机真实配置跑一次预检，验证模块目录解析。
//
// 默认跳过（依赖本机配置与代码目录）：
//
//	WC_REAL_CFG=1 go test . -run TestPrecheckWithRealConfig -v
//
// 刻意不打印任何 AI 凭据字段——只输出与部署相关的路径信息。
func TestPrecheckWithRealConfig(t *testing.T) {
	if os.Getenv("WC_REAL_CFG") != "1" {
		t.Skip("未设置 WC_REAL_CFG=1，跳过")
	}

	cfg, _, err := config.Load()
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	d := cfg.Deploy
	if d.BasePath == "" && len(d.Projects) == 0 {
		t.Skip("部署配置为空，跳过")
	}

	t.Logf("代码根: %s", d.BasePath)
	t.Logf("输出目录: %s", d.OutputDir)
	t.Logf("保留环境: %s", d.KeepEnv)
	for _, p := range d.Projects {
		t.Logf("项目 %s（root=%s）:", p.Name, p.Root)
		for _, m := range p.Modules {
			t.Logf("    %-24s type=%-9s root=%-24s script=%s",
				m.Name, m.Type, m.Root, m.Script)
		}
	}

	res := deploy.Precheck(d, deploy.RunOptions{OutputDir: d.OutputDir})
	bad := 0
	for _, it := range res.Items {
		mark := "✓"
		if !it.OK {
			mark = "✗"
			if it.Critical {
				bad++
			}
		}
		t.Logf("%s %-28s %s", mark, it.Name, it.Detail)
		if !it.OK && it.Fix != "" {
			t.Logf("      → %s", it.Fix)
		}
	}
	t.Logf("预检结论: OK=%v，关键未通过=%d，将产出 %d 个 zip", res.OK, bad, len(res.Planned))
	for _, pl := range res.Planned {
		t.Logf("    产出: %s", pl.Path)
	}

	// 关键断言：模块目录必须都能解析到真实存在的目录
	// （这正是早期把 Root 写成相对代码根时失败的地方）
	if len(res.Planned) == 0 {
		t.Errorf("没有解析出任何待产出产物，说明模块目录解析仍有问题")
	}
}
