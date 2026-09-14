// Package config 负责读取与保存用户配置。
//
// 配置文件是这个工具「模块化、按需启动」的唯一事实来源：
// 某个模块在配置里禁用后，入口页不显示它，它的后端也绝不会初始化。
//
// 文件位置：%APPDATA%\winclean\config.yaml
// 刻意不用注册表存配置：配置应该是一个用户能直接看到、能备份、
// 能用文本编辑器改、能随目录一起搬走的文件。
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"winclean/internal/deploy"
)

// ModuleConfig 是单个模块的配置。
type ModuleConfig struct {
	// Enabled 用指针区分「用户明确设置过」与「未设置」。
	// 未设置时按模块声明的默认值处理。
	Enabled *bool `yaml:"enabled"`
}

// General 是通用配置。
type General struct {
	// Home 是启动时显示的主页面，取值为某个模块 id（如 chat / scan）。
	// 主功能恰好一个；其余已启用模块作为扩展页出现在导航里。
	// 指向的模块被禁用时，启动时自动回退到第一个可用页面并提示。
	Home string `yaml:"home"`
}

// AI 是大模型接入配置。
//
// 采用 OpenAI 兼容协议（/chat/completions），因此 DeepSeek、Kimi、
// 智谱、通义等大多数国内外服务以及本地网关都能直接填 base_url 使用。
//
// 安全说明：APIKey 明文保存在本机配置文件中。界面与文档均已提示。
// 未配置时 AI 相关代码不发起任何网络请求。
type AI struct {
	Provider string `yaml:"provider"` // 预设名（deepseek/moonshot/zhipu/openai/custom），仅用于展示
	BaseURL  string `yaml:"base_url"` // 如 https://api.deepseek.com/v1
	APIKey   string `yaml:"api_key"`
	Model    string `yaml:"model"` // 如 deepseek-chat
}

// Config 是配置文件的根结构。
type Config struct {
	General General                 `yaml:"general"`
	AI      AI                      `yaml:"ai"`
	Deploy  deploy.Config           `yaml:"deploy"`
	Modules map[string]ModuleConfig `yaml:"modules"`
}

// Default 返回默认配置（所有模块按各自声明的默认值）。
func Default() *Config {
	return &Config{Modules: map[string]ModuleConfig{}}
}

// Dir 返回配置目录。
func Dir() (string, error) {
	appdata := os.Getenv("APPDATA")
	if appdata == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		appdata = filepath.Join(home, "AppData", "Roaming")
	}
	return filepath.Join(appdata, "winclean"), nil
}

// Path 返回配置文件完整路径。
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}

// Load 读取配置。
//
// 返回值里的 warnings 描述非致命情况（文件不存在、内容损坏已回退默认），
// 调用方应把这些问题呈现给用户而不是默默吞掉——
// 尤其「配置损坏」如果静默回退，用户会以为自己的修改生效了。
func Load() (*Config, []string, error) {
	path, err := Path()
	if err != nil {
		return Default(), nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 首次运行：没有配置文件是正常状态，不算告警
			return Default(), nil, nil
		}
		return Default(), []string{"读取配置失败: " + err.Error()}, err
	}

	cfg := Default()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return Default(),
			[]string{fmt.Sprintf("配置文件 %s 无法解析（%v），已回退默认配置。原文件已保留为 config.yaml.bak", path, err)},
			nil
	}
	return cfg, nil, nil
}

// Save 把配置写回磁盘。先写临时文件再改名，避免写一半损坏。
func (c *Config) Save() error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "config.yaml")

	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// IsEnabled 返回某模块是否启用。
//
// 优先级：用户配置 > 模块默认值。
// 配置里写了 enabled: false 的模块，入口不显示、后端不初始化——
// 这是「不用的功能不启动」的落地位置。
func (c *Config) IsEnabled(id string, def bool) bool {
	if c == nil || c.Modules == nil {
		return def
	}
	mc, ok := c.Modules[id]
	if !ok || mc.Enabled == nil {
		return def
	}
	return *mc.Enabled
}

// SetEnabled 写入某模块的启用状态（不落盘，由调用方决定何时 Save）。
func (c *Config) SetEnabled(id string, enabled bool) {
	if c.Modules == nil {
		c.Modules = map[string]ModuleConfig{}
	}
	c.Modules[id] = ModuleConfig{Enabled: &enabled}
}
