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

	if len(list) != 4 {
		t.Fatalf("应注册 4 个模块，实际 %d", len(list))
	}
	for _, m := range list {
		if m.Meta.ID == "" {
			t.Errorf("存在空 ID 的模块: %+v", m.Meta)
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
