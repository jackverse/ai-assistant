package main

import (
	"context"
	"os"
	"testing"
	"time"

	"winclean/internal/ai"
	"winclean/internal/config"
)

// TestLiveAIStream 用本机真实配置向所配置的模型发一次流式请求。
//
// 默认跳过（需要网络和你自己的 API Key），显式开启：
//
//	WC_LIVE_AI=1 go test . -run TestLiveAIStream -v
//
// 存在的意义：排除 GUI 与前端因素，单独验证「配置读取 → HTTP 流式请求 →
// 增量解析」这条链路。当界面里聊天不工作时，先跑这个就能定位是
// 链路问题还是界面问题。
func TestLiveAIStream(t *testing.T) {
	if os.Getenv("WC_LIVE_AI") != "1" {
		t.Skip("未设置 WC_LIVE_AI=1，跳过真实请求")
	}

	cfg, warns, err := config.Load()
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	for _, w := range warns {
		t.Logf("配置告警: %s", w)
	}

	s := ai.Settings{BaseURL: cfg.AI.BaseURL, APIKey: cfg.AI.APIKey, Model: cfg.AI.Model}
	if !s.Valid() {
		t.Fatalf("AI 配置不完整: base_url=%q model=%q has_key=%v",
			s.BaseURL, s.Model, s.APIKey != "")
	}
	t.Logf("目标: %s  模型: %s", s.BaseURL, s.Model)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var deltas int
	start := time.Now()
	reply, err := ai.Stream(ctx, s, []ai.Message{
		{Role: "user", Content: "你好,请用一句话介绍你自己"},
	}, func(d string) {
		deltas++
	})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}

	t.Logf("耗时 %s，收到 %d 个增量片段", time.Since(start).Round(time.Millisecond), deltas)
	t.Logf("回复:\n%s", reply)

	if reply == "" {
		t.Error("回复为空")
	}
}
