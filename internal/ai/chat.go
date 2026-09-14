// Package ai 提供大模型对话的最小接入层。
//
// 设计约束（来自用户需求与 14 号设计文档）：
//   - 采用 OpenAI 兼容协议（/chat/completions，流式），因此 DeepSeek、Kimi、
//     智谱、通义以及各类本地网关都能只填 base_url 直接使用；
//   - 未配置时调用方根本不应走到这里——本包不做任何配置检查之外的兜底，
//     也不会在程序启动时产生任何网络活动；
//   - 只用标准库 net/http，不引入任何 SDK。
package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Message 是一条对话消息。Role 为 system/user/assistant。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Settings 是一次请求所需的连接信息。
type Settings struct {
	BaseURL string
	APIKey  string
	Model   string
}

// Valid 报告配置是否齐全。
func (s Settings) Valid() bool {
	return s.BaseURL != "" && s.APIKey != "" && s.Model != ""
}

// Preset 是服务商预设，让用户不必手填 base_url 与模型名。
type Preset struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
	Note    string `json:"note"`
}

// Presets 是内置服务商预设。
//
// base_url 均为官方公开的 OpenAI 兼容入口；模型名取各家的入门档，
// 用户在设置页可以改成任何该服务支持的模型。
func Presets() []Preset {
	return []Preset{
		{ID: "deepseek", Name: "DeepSeek", BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-chat",
			Note: "需在 platform.deepseek.com 申请 API Key"},
		{ID: "moonshot", Name: "Kimi (Moonshot)", BaseURL: "https://api.moonshot.cn/v1", Model: "moonshot-v1-8k",
			Note: "需在 platform.moonshot.cn 申请 API Key"},
		{ID: "zhipu", Name: "智谱 GLM", BaseURL: "https://open.bigmodel.cn/api/paas/v4", Model: "glm-4-flash",
			Note: "glm-4-flash 免费，适合先用起来"},
		{ID: "qwen", Name: "通义千问", BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", Model: "qwen-plus",
			Note: "使用 DashScope 的兼容模式入口"},
		{ID: "openai", Name: "OpenAI", BaseURL: "https://api.openai.com/v1", Model: "gpt-4o-mini",
			Note: "境内网络通常需要自备代理"},
		{ID: "custom", Name: "自定义 / 本地网关", BaseURL: "", Model: "",
			Note: "任何 OpenAI 兼容服务，例如 Ollama: http://127.0.0.1:11434/v1"},
	}
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
}

type chatResponse struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Stream 发起流式对话，把增量文本实时回调给 onDelta，返回完整回复。
//
// 通过 SSE 解析 "data: ..." 行；收到 [DONE] 结束。
// ctx 取消（用户点停止/关窗口）会中断请求与读取。
func Stream(ctx context.Context, s Settings, messages []Message, onDelta func(string)) (string, error) {
	if !s.Valid() {
		return "", fmt.Errorf("AI 尚未配置完整（需要 base_url、api_key、model）")
	}
	if !strings.HasSuffix(s.BaseURL, "/") {
		s.BaseURL += "/"
	}
	url := s.BaseURL + "chat/completions"

	payload, err := json.Marshal(chatRequest{Model: s.Model, Messages: messages, Stream: true})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.APIKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("连接失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var buf strings.Builder
		_, _ = fmt.Fprint(&buf, readErrBody(resp))
		return "", fmt.Errorf("服务返回 %s: %s", resp.Status, trunc(buf.String(), 300))
	}

	var full strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}

		var cr chatResponse
		if err := json.Unmarshal([]byte(data), &cr); err != nil {
			continue // 跳过无法解析的心跳/注释行，不让整次对话失败
		}
		if cr.Error != nil {
			return full.String(), fmt.Errorf("服务错误: %s", cr.Error.Message)
		}
		if len(cr.Choices) > 0 {
			delta := cr.Choices[0].Delta.Content
			if delta != "" {
				full.WriteString(delta)
				if onDelta != nil {
					onDelta(delta)
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return full.String(), fmt.Errorf("读取响应中断: %w", err)
	}
	if full.Len() == 0 {
		return "", fmt.Errorf("服务没有返回内容（请检查模型名是否正确）")
	}
	return full.String(), nil
}

// Test 用一条极短消息验证配置是否可用。
func Test(ctx context.Context, s Settings) error {
	_, err := Stream(ctx, s, []Message{{Role: "user", Content: "ping"}}, nil)
	return err
}

func readErrBody(resp *http.Response) string {
	buf := new(strings.Builder)
	limit := int64(2048)
	if resp.ContentLength > 0 && resp.ContentLength < limit {
		limit = resp.ContentLength
	}
	body := make([]byte, limit)
	n, _ := resp.Body.Read(body)
	buf.Write(body[:n])
	return buf.String()
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
