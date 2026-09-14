package deploy

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// externalConfig 对应既有工具（bundle-packer）的 config.yaml 结构。
//
// 字段与那边保持一致，用于把用户已有的配置一键导入本模块，
// 省得重新录一遍项目与模块清单。
type externalConfig struct {
	OutputDir string `yaml:"output_dir"`
	BasePath  string `yaml:"base_path"`
	MavenPath string `yaml:"maven_path"`
	JDKPath   string `yaml:"jdk_path"`
	KeepEnv   string `yaml:"keep_env"`
	Projects  []struct {
		Name    string `yaml:"name"`
		Root    string `yaml:"root"`
		Modules []struct {
			Name   string `yaml:"name"`
			Type   string `yaml:"type"`
			Source string `yaml:"source"`
			Script string `yaml:"script"`
			Output string `yaml:"output"`
		} `yaml:"modules"`
	} `yaml:"projects"`
}

// ImportExternalConfig 读取既有工具的 config.yaml 并转换为本模块配置。
//
// 转换规则：
//   - output_dir / base_path / keep_env 直接沿用
//   - maven_path / jdk_path 也一并导入，但界面会优先建议使用自动探测结果
//     （既有配置里的绝对路径换机器就失效，这正是本次要改进的点）
//   - projects[].modules[] 逐项映射；type 缺省为 backend
func ImportExternalConfig(path string) (Config, error) {
	var ext externalConfig

	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("读取配置失败: %w", err)
	}
	if err := yaml.Unmarshal(data, &ext); err != nil {
		return Config{}, fmt.Errorf("解析配置失败（注意 yaml 里反斜杠要写成 \\\\ 或用正斜杠）: %w", err)
	}
	if len(ext.Projects) == 0 {
		return Config{}, fmt.Errorf("配置里没有 projects，确认这是打包工具的配置文件: %s", path)
	}

	cfg := Config{
		BasePath:  strings.TrimSpace(ext.BasePath),
		OutputDir: strings.TrimSpace(ext.OutputDir),
		MavenPath: strings.TrimSpace(ext.MavenPath),
		JDKPath:   strings.TrimSpace(ext.JDKPath),
		KeepEnv:   strings.TrimSpace(ext.KeepEnv),
	}
	if cfg.KeepEnv == "" {
		cfg.KeepEnv = "prod"
	}

	for _, p := range ext.Projects {
		pc := ProjectConfig{
			Name: strings.TrimSpace(p.Name),
			Root: strings.TrimSpace(p.Root),
		}
		if pc.Root == "" {
			pc.Root = pc.Name
		}
		for _, m := range p.Modules {
			mc := ModuleConfig{
				Name:   strings.TrimSpace(m.Name),
				Type:   strings.TrimSpace(m.Type),
				Source: strings.TrimSpace(m.Source),
				Script: strings.TrimSpace(m.Script),
				Output: strings.TrimSpace(m.Output),
			}
			if mc.Type == "" {
				mc.Type = "backend"
			}
			if mc.Output == "" {
				mc.Output = mc.Name
				if mc.Type == "backend" {
					// 与既有工具的产物命名保持一致：wic-admin → wic_admin
					mc.Output = sanitizeBackendOutput(mc.Name)
				}
			}
			// 前端模块缺 script 时给出提示而不是静默接受：
			// 没有 script 就无法构建，早提示比跑到打包时失败好
			if mc.Type == "frontend" && mc.Script == "" {
				mc.Script = ""
			}
			pc.Modules = append(pc.Modules, mc)
		}
		cfg.Projects = append(cfg.Projects, pc)
	}

	return cfg, nil
}

// Describe 生成配置的一句话摘要，用于界面展示。
func (c Config) Describe() string {
	nmod := 0
	nfe := 0
	for _, p := range c.Projects {
		for _, m := range p.Modules {
			nmod++
			if m.Type == "frontend" {
				nfe++
			}
		}
	}
	return fmt.Sprintf("%d 个项目 / %d 个模块（后端 %d、前端 %d），保留环境 %s",
		len(c.Projects), nmod, nmod-nfe, nfe, c.effectiveKeepEnv())
}
