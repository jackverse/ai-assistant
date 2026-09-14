package main

import (
	"encoding/json"
	"testing"

	"winclean/internal/config"
)

// TestRegistryListJSON 验证模块注册表能产出前端所需的 JSON。
//
// 背景：界面出现过「导航为空」的现象，需要区分是 Go 侧没产出数据，
// 还是前端序列化/字段名的问题。这个测试固定 Go 侧行为。
func TestRegistryListJSON(t *testing.T) {
	cfg := config.Default()
	app := NewApp(cfg)

	list := app.reg.List()
	b, _ := json.MarshalIndent(list, "", "  ")
	t.Logf("模块数: %d\n%s", len(list), b)

	// 断言必需模块存在，而不是硬编码总数——
	// 否则每加一个功能入口都要改测试（本文件就被 deploy 模块撞过一次）
	required := []string{"chat", "scan", "deploy", "clean", "migrate"}
	byID := map[string]bool{}
	for _, m := range list {
		if m.Meta.ID == "" {
			t.Errorf("存在空 ID 的模块: %+v", m.Meta)
		}
		byID[m.Meta.ID] = true
	}
	for _, id := range required {
		if !byID[id] {
			t.Errorf("缺少必需模块 %q（已注册: %v）", id, keysOf(byID))
		}
	}

	// 默认启用状态：全部功能模块默认启用。
	// clean/migrate 曾因「未实现」默认关闭；现已实现并上线，
	// 反转断言防止将来有人无意把它们改回禁用。
	enabled := map[string]bool{}
	for _, m := range list {
		enabled[m.Meta.ID] = m.Enabled
	}
	for _, id := range []string{"chat", "scan", "deploy", "clean", "migrate"} {
		if !enabled[id] {
			t.Errorf("%s 应默认启用", id)
		}
	}

	// 主页解析：默认配置（AI 未配置）应回退到 scan
	if got := app.resolveHome(); got != "scan" {
		t.Errorf("AI 未配置时主页应回退 scan，实际 %s", got)
	}

	// 配置 AI 后，默认主页应为 chat
	cfg.AI = config.AI{Provider: "deepseek", BaseURL: "https://api.deepseek.com/v1",
		APIKey: "sk-test", Model: "deepseek-chat"}
	app2 := NewApp(cfg)
	if got := app2.resolveHome(); got != "chat" {
		t.Errorf("AI 已配置时主页应为 chat，实际 %s", got)
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
