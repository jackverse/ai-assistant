package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"winclean/internal/config"
	"winclean/internal/deploy"
)

// TestRealPackOneFrontendModule 用本机真实配置打包一个前端模块。
//
// 默认跳过（依赖本机代码与 Node）：
//
//	WC_REAL_PACK=1 go test . -run TestRealPackOneFrontendModule -v -timeout 20m
//
// 刻意输出到【临时目录】而不是配置里的产物目录——验证链路时不该
// 覆盖用户已有的部署产物。
//
// 这个用例同时是「打包不弹 cmd 窗口」的观测点：跑的时候如果弹出黑窗，
// 说明 CREATE_NO_WINDOW 没生效。
func TestRealPackOneFrontendModule(t *testing.T) {
	if os.Getenv("WC_REAL_PACK") != "1" {
		t.Skip("未设置 WC_REAL_PACK=1，跳过")
	}

	cfg, _, err := config.Load()
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	d := cfg.Deploy
	if len(d.Projects) == 0 {
		t.Skip("部署配置为空")
	}

	// 只取第一个前端模块（前端比后端快得多），输出到临时目录
	var (
		proj deploy.ProjectConfig
		mod  deploy.ModuleConfig
	)
	for _, p := range d.Projects {
		for _, m := range p.Modules {
			if m.Type == "frontend" {
				proj, mod = p, m
				break
			}
		}
		if mod.Name != "" {
			break
		}
	}
	if mod.Name == "" {
		t.Skip("配置里没有前端模块")
	}

	outDir := t.TempDir()
	t.Logf("打包 %s/%s（脚本 %q）→ %s", proj.Name, mod.Name, mod.Script, outDir)

	// 用临时局部的配置副本，避免污染原配置
	local := d
	local.Projects = []deploy.ProjectConfig{{
		Name: proj.Name, Root: proj.Root, Modules: []deploy.ModuleConfig{mod},
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	start := time.Now()
	var lines []string
	err = deploy.Run(ctx, local, deploy.RunOptions{
		Project: proj.Name, Modules: []string{mod.Name}, OutputDir: outDir,
	}, func(ev deploy.Event) {
		if ev.Line != "" {
			lines = append(lines, ev.Line)
		}
	})

	// 只回显最后若干行，避免刷屏
	tail := lines
	if len(tail) > 25 {
		tail = tail[len(tail)-25:]
	}
	for _, l := range tail {
		t.Logf("  | %s", l)
	}

	if err != nil {
		t.Fatalf("打包失败: %v", err)
	}

	zipPath := filepath.Join(outDir, mod.Output+".zip")
	if !strings.HasSuffix(mod.Output, ".zip") {
		if _, statErr := os.Stat(zipPath); statErr != nil {
			// 产物名可能与推测不同，列出实际产出
			entries, _ := os.ReadDir(outDir)
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Fatalf("未找到 %s；实际产出: %v", zipPath, names)
		}
	}
	st, _ := os.Stat(zipPath)
	t.Logf("✅ 打包成功：%s（%.1f MB），耗时 %s",
		zipPath, float64(st.Size())/1024/1024, time.Since(start).Round(time.Second))
}
