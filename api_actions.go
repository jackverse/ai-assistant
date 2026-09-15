package main

// 对外能力接口的注册（见 docs/design/22-对外能力接口与AI可控制规范.md）。
//
// 规则：每个在 registerModules() 注册的模块都必须在这里至少暴露一个动作。
// 有强制测试把关（api_compliance_test.go），漏了会被测试拦下。
//
// 本文件是 main 包唯一新增的对外接口层：Headless 模式下（`助手.exe api ...`）
// 也能工作——因此 handler 里绝不依赖 wails 的 ctx，也不读界面状态。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"winclean/internal/ai"
	"winclean/internal/api"
	"winclean/internal/report"
	"winclean/internal/scan"
)

// registerAllActions 把应用的全部能力注册进对外接口表。
func registerAllActions(app *App, reg *api.Registry) {
	// ① 每个模块自动获得 <module>.info —— 保证「每个模块都有对外接口」恒成立，
	//    也让 AI 能先枚举模块再看各自能做什么。
	for _, m := range app.reg.List() {
		mod := m
		reg.Register(api.Action{
			ID:     mod.Meta.ID + ".info",
			Module: mod.Meta.ID,
			Kind:   api.KindReadonly,
			Summary: fmt.Sprintf("查看「%s」模块信息：%s（当前%s，状态 %s）",
				mod.Meta.Name, mod.Meta.Desc,
				map[bool]string{true: "已启用", false: "已禁用"}[mod.Enabled], mod.Meta.Status),
			Handler: func(ctx context.Context, args map[string]any) (any, error) {
				return map[string]any{
					"id": mod.Meta.ID, "name": mod.Meta.Name, "desc": mod.Meta.Desc,
					"enabled": mod.Enabled, "opened": mod.Opened, "status": mod.Meta.Status,
				}, nil
			},
		})
	}

	registerChatActions(app, reg)
	registerDiskActions(app, reg)
	registerDeployActions(app, reg)
}

// ───────── chat ─────────

func registerChatActions(app *App, reg *api.Registry) {
	reg.Register(api.Action{
		ID: "chat.ask", Module: "chat", Kind: api.KindReadonly,
		Summary: "向已配置的大模型提问并返回回答（同步等待，可能耗时数十秒）",
		Params: []api.Param{
			{Name: "prompt", Type: "string", Required: true, Desc: "要问的问题", Example: "C 盘快满了怎么办"},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			s := app.scanAISettings()
			if !s.Valid() {
				return nil, fmt.Errorf("AI 尚未配置：请先在设置里填写 base_url / api_key / model")
			}
			reply, err := ai.Stream(ctx, s, []ai.Message{
				{Role: "user", Content: str(args, "prompt")},
			}, nil)
			if err != nil {
				return nil, err
			}
			return map[string]any{"reply": strings.TrimSpace(reply)}, nil
		},
	})

	reg.Register(api.Action{
		ID: "chat.analyze_scan", Module: "chat", Kind: api.KindReadonly,
		Summary: "用 AI 分析最近一次扫描结果：判断能删/不能删、是否在用、最近使用时间（同步等待，可能耗时数分钟）",
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			app.mu.Lock()
			res := app.result
			app.mu.Unlock()
			if res == nil {
				return nil, fmt.Errorf("还没有扫描结果：先调用 scan.start 并等待完成")
			}
			s := app.scanAISettings()
			if !s.Valid() {
				return nil, fmt.Errorf("AI 尚未配置：请先在设置里填写 base_url / api_key / model")
			}
			digest := buildScanDigest(res, probeTopFiles(res.LargestFiles, 20))
			reply, err := ai.Stream(ctx, s, []ai.Message{
				{Role: "system", Content: scanAISysPrompt},
				{Role: "user", Content: "扫描事实：\n" + digest + "\n\n请按规则输出分析。"},
			}, nil)
			if err != nil {
				return nil, err
			}
			return map[string]any{"analysis": strings.TrimSpace(reply)}, nil
		},
	})
}

// ───────── disk（扫描 / 清理 / 迁移，三合一模块）─────────

func registerDiskActions(app *App, reg *api.Registry) {
	// 同步扫描：CLI 形态下的主力动作。
	//
	// 为什么必须有它：`助手.exe api call` 每次都是**新进程**，状态不跨进程保留——
	// 「启动扫描」后另起进程查进度永远是空的（实测踩过）。
	// 因此 CLI 形态必须提供「一次调用做完一件事」的同步动作。
	reg.Register(api.Action{
		ID: "disk.scan", Module: "disk", Kind: api.KindReadonly,
		Summary: "同步扫描磁盘并等待完成，返回结果摘要（推荐用法：之后用 disk.decision 取三色清单）",
		Params: []api.Param{
			{Name: "disk", Type: "string", Required: true, Desc: "盘符字母，如 C", Example: "C"},
			{Name: "deep", Type: "bool", Required: false, Desc: "深度模式：逐文件读真实占用并去重硬链接，较慢", Example: "false"},
			{Name: "timeout_sec", Type: "int", Required: false, Desc: "超时秒数，默认 1800", Example: "900"},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			if err := app.StartScan(str(args, "disk"), boolArg(args, "deep")); err != nil {
				return nil, err
			}
			return awaitScan(ctx, app, time.Duration(intArg(args, "timeout_sec", 1800))*time.Second)
		},
	})

	// 同步扫描指定目录：AI 日常最需要的形态（扫一个目录，秒级返回）
	reg.Register(api.Action{
		ID: "disk.scan_path", Module: "disk", Kind: api.KindReadonly,
		Summary: "同步扫描指定目录（含子目录）并返回占用摘要：回答「这个文件夹占了多少、里面什么最大」",
		Params: []api.Param{
			{Name: "path", Type: "string", Required: true, Desc: "要扫描的目录绝对路径", Example: `D:\code\wic-sh`},
			{Name: "max_depth", Type: "int", Required: false, Desc: "上报的目录层级上限，默认 3", Example: "3"},
			{Name: "top_files", Type: "int", Required: false, Desc: "返回的最大文件数，默认 10", Example: "10"},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			root := str(args, "path")
			if root == "" {
				return nil, fmt.Errorf("缺少参数 path")
			}
			res, err := scan.Run(ctx, scan.Options{
				Roots:    []string{root},
				MaxDepth: intArg(args, "max_depth", 3),
				MinSize:  1 << 20,
				TopFiles: intArg(args, "top_files", 10),
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"root":          root,
				"bytes_on_disk": res.Stats.BytesOnDisk,
				"files":         res.Stats.FilesScanned,
				"dirs":          res.Stats.DirsScanned,
				"duration_ms":   res.DurationMS,
				"top_dirs":      res.Dirs,
				"largest_files": res.LargestFiles,
			}, nil
		},
	})

	reg.Register(api.Action{
		ID: "disk.scan_start", Module: "disk", Kind: api.KindMutating,
		Summary: "仅启动后台扫描并立即返回（⚠️ CLI 下状态不跨进程，查不到进度，请改用同步的 disk.scan；本动作用于长驻服务模式）",
		Params: []api.Param{
			{Name: "disk", Type: "string", Required: true, Desc: "盘符字母，如 C", Example: "C"},
			{Name: "deep", Type: "bool", Required: false, Desc: "深度模式", Example: "false"},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			if err := app.StartScan(str(args, "disk"), boolArg(args, "deep")); err != nil {
				return nil, err
			}
			return map[string]any{"started": true,
				"hint": "CLI 下请改用 disk.scan（同步等待）；本动作需配合长驻服务才能查进度"}, nil
		},
	})

	reg.Register(api.Action{
		ID: "disk.scan_status", Module: "disk", Kind: api.KindReadonly,
		Summary: "查询扫描进度（⚠️ 仅长驻服务模式下有效；CLI 每次调用是新进程，取不到正在跑的扫描）",
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			return app.Progress(), nil
		},
	})

	reg.Register(api.Action{
		ID: "disk.scan_cancel", Module: "disk", Kind: api.KindMutating,
		Summary: "取消正在进行的扫描",
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			app.CancelScan()
			return map[string]any{"cancelled": true}, nil
		},
	})

	reg.Register(api.Action{
		ID: "disk.result", Module: "disk", Kind: api.KindReadonly,
		Summary: "取最近一次扫描的原始结果（目录占用与最大文件，数据量较大）",
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			app.mu.Lock()
			res := app.result
			app.mu.Unlock()
			if res == nil {
				return nil, fmt.Errorf("还没有扫描结果：先调用 disk.scan_start")
			}
			return res, nil
		},
	})

	reg.Register(api.Action{
		ID: "disk.decision", Module: "disk", Kind: api.KindReadonly,
		Summary: "三色决策清单：可以删除 / 需要你决定 / 不要动（每项含人话原因与迁移方法）",
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			return app.BuildDecisionList()
		},
	})

	reg.Register(api.Action{
		ID: "disk.space_report", Module: "disk", Kind: api.KindReadonly,
		Summary: "空间报告：按「系统 / 已安装 / 开发缓存 / 用户数据 / 可清理 / 未识别」分组的占用概览",
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			return app.BuildSpaceReport()
		},
	})

	reg.Register(api.Action{
		ID: "disk.drilldown", Module: "disk", Kind: api.KindReadonly,
		Summary: "展开某个目录的直接子项，用于核对判断是否可信（限最近一次扫描结果中出现过的路径）",
		Params: []api.Param{
			{Name: "path", Type: "string", Required: true, Desc: "目录绝对路径", Example: `C:\Users\me\AppData\Local\Temp`},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			return app.DrillDown(str(args, "path"))
		},
	})

	// ── 清理 ──
	reg.Register(api.Action{
		ID: "disk.clean_scan", Module: "disk", Kind: api.KindReadonly,
		Summary: "扫描可清理项（只读，不删任何东西）",
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			if err := app.CleanScan(); err != nil {
				return nil, err
			}
			return map[string]any{"started": true, "hint": "用 disk.clean_items 取候选清单"}, nil
		},
	})
	reg.Register(api.Action{
		ID: "disk.clean_items", Module: "disk", Kind: api.KindReadonly,
		Summary: "列出清理候选（每项含大小、等级、原因）",
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			return app.CleanItems(), nil
		},
	})
	reg.Register(api.Action{
		ID: "disk.clean_execute", Module: "disk", Kind: api.KindDestructive,
		Summary: "删除指定的清理项（会真实删除文件，且不进回收站）",
		Params: []api.Param{
			{Name: "ids", Type: "strings", Required: true, Desc: "要删除的候选 id 列表", Example: `["temp-user"]`},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			if err := app.CleanExecute(strSlice(args, "ids"), nil, true); err != nil {
				return nil, err
			}
			return map[string]any{"started": true, "hint": "用 disk.clean_progress 查进度与结果"}, nil
		},
	})
	reg.Register(api.Action{
		ID: "disk.clean_progress", Module: "disk", Kind: api.KindReadonly,
		Summary: "查询清理执行进度与结果",
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			return map[string]any{
				"progress": app.CleanProgress(),
				"results":  app.CleanResults(),
			}, nil
		},
	})

	// ── 迁移 ──
	reg.Register(api.Action{
		ID: "disk.migrate_scan", Module: "disk", Kind: api.KindReadonly,
		Summary: "扫描可迁移项（只读，不动任何文件）",
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			if err := app.MigrateScan(); err != nil {
				return nil, err
			}
			return map[string]any{"started": true, "hint": "用 disk.migrate_items 取清单"}, nil
		},
	})
	reg.Register(api.Action{
		ID: "disk.migrate_items", Module: "disk", Kind: api.KindReadonly,
		Summary: "列出可迁移项（含建议目标、机制、风险）",
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			return app.MigrateItems(), nil
		},
	})
	reg.Register(api.Action{
		ID: "disk.migrate_execute", Module: "disk", Kind: api.KindDestructive,
		Summary: "执行迁移：复制到目标盘并建立目录联接（会移动真实数据，请先确认计划）",
		Params: []api.Param{
			{Name: "ids", Type: "strings", Required: true, Desc: "要迁移的项 id 列表",
				Example: `["yarn-cache","maven-repo"]`},
			{Name: "target_base", Type: "string", Required: true, Desc: "目标根目录", Example: `D:\Moved`},
			{Name: "set_env", Type: "bool", Required: false, Desc: "同时写入环境变量（仅环境变量类项适用）"},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			if err := app.MigrateExecute(strSlice(args, "ids"), str(args, "target_base"),
				boolArg(args, "set_env"), true); err != nil {
				return nil, err
			}
			return map[string]any{"started": true, "hint": "用 disk.migrate_progress 查进度"}, nil
		},
	})

	reg.Register(api.Action{
		ID: "disk.export_report", Module: "disk", Kind: api.KindMutating,
		Summary: "把最近一次扫描导出为 HTML 报告并写入指定文件（Headless 下不会自动打开浏览器）",
		Params: []api.Param{
			{Name: "output", Type: "string", Required: false, Desc: "输出路径", Example: `D:\report.html`},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			return app.exportReportTo(str(args, "output"))
		},
	})
}

// ───────── deploy（Java 开发与打包）─────────

func registerDeployActions(app *App, reg *api.Registry) {
	reg.Register(api.Action{
		ID: "deploy.toolchain", Module: "deploy", Kind: api.KindReadonly,
		Summary: "探测本机 JDK 与 Maven（含版本、路径、来源、是否可用）",
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			return app.DetectToolchain(nil), nil
		},
	})

	reg.Register(api.Action{
		ID: "deploy.projects", Module: "deploy", Kind: api.KindReadonly,
		Summary: "列出已配置的打包项目与模块（含类型、构建脚本、产物名）",
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			return app.GetDeploy(), nil
		},
	})

	reg.Register(api.Action{
		ID: "deploy.discover", Module: "deploy", Kind: api.KindReadonly,
		Summary: "扫描代码根目录，自动发现 Maven 聚合项目与前端模块",
		Params: []api.Param{
			{Name: "code_root", Type: "string", Required: false, Desc: "代码根目录（留空用配置里的 base_path）", Example: `D:\code`},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			return app.DiscoverDeployProjects(str(args, "code_root"))
		},
	})

	reg.Register(api.Action{
		ID: "deploy.precheck", Module: "deploy", Kind: api.KindReadonly,
		Summary: "打包前预检：逐项检查工具链、目录、模块是否就绪，并列出将产出的产物",
		Params: []api.Param{
			{Name: "project", Type: "string", Required: false, Desc: "项目名（留空检查全部）"},
			{Name: "output_dir", Type: "string", Required: false, Desc: "产物输出目录（留空用配置）"},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			return app.PrecheckDeploy(str(args, "project"), strSlice(args, "modules"), str(args, "output_dir"))
		},
	})

	reg.Register(api.Action{
		ID: "deploy.pack", Module: "deploy", Kind: api.KindMutating,
		Summary: "执行打包：后端 mvn clean package 并整理 war_exploded，前端 npm run 后压缩产物",
		Params: []api.Param{
			{Name: "project", Type: "string", Required: false, Desc: "项目名（留空打全部）"},
			{Name: "output_dir", Type: "string", Required: false, Desc: "产物输出目录（留空用配置）"},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			if err := app.StartPack(str(args, "project"), strSlice(args, "modules"),
				str(args, "output_dir")); err != nil {
				return nil, err
			}
			return map[string]any{"started": true, "hint": "用 deploy.pack_status 查进度与日志"}, nil
		},
	})

	reg.Register(api.Action{
		ID: "deploy.pack_status", Module: "deploy", Kind: api.KindReadonly,
		Summary: "查询打包进度与最近日志",
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			return app.PackStatus(), nil
		},
	})

	// Java 开发：服务启停与 Git 变更
	reg.Register(api.Action{
		ID: "deploy.dev_services", Module: "deploy", Kind: api.KindReadonly,
		Summary: "探测本机开发服务状态（端口占用、进程）",
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			return app.DetectAllDevServices()
		},
	})
	reg.Register(api.Action{
		ID: "deploy.git_changes", Module: "deploy", Kind: api.KindReadonly,
		Summary: "查看工作区的 Git 变更（分支、改动文件）",
		Params: []api.Param{
			{Name: "project", Type: "string", Required: false, Desc: "项目名（留空用当前）"},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			return app.GitChanges(), nil
		},
	})
}

// ───────── 参数辅助 ─────────

func str(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func boolArg(args map[string]any, key string) bool {
	switch v := args[key].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1"
	}
	return false
}

func intArg(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	case string:
		n := 0
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

// awaitScan 等待后台扫描结束，返回结果摘要。
//
// 同步语义是刻意的：CLI 每次调用都是新进程，无法跨进程轮询，
// 因此必须「调用即等到完成」。
func awaitScan(ctx context.Context, app *App, timeout time.Duration) (any, error) {
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			app.CancelScan()
			return nil, fmt.Errorf("调用被取消")
		case <-tick.C:
			p := app.Progress()
			if p.Running {
				if time.Now().After(deadline) {
					app.CancelScan()
					return nil, fmt.Errorf("扫描超时（超过 %v）", timeout)
				}
				continue
			}
			res := app.Result()
			if res == nil {
				return nil, fmt.Errorf("扫描未产出结果（可能启动失败）")
			}
			app.mu.Lock()
			s := res.Stats
			app.mu.Unlock()
			return map[string]any{
				"files_scanned": s.FilesScanned,
				"dirs_scanned":  s.DirsScanned,
				"bytes_on_disk": s.BytesOnDisk,
				"skipped_dirs":  s.SkippedDirs,
				"duration_ms":   res.DurationMS,
				"mode":          res.Mode,
				"hint":          "用 disk.decision 取三色清单（能删/需确认/不要动），或 disk.space_report 看分组概览",
			}, nil
		}
	}
}

func strSlice(args map[string]any, key string) []string {
	v, ok := args[key]
	if !ok {
		return nil
	}
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		if strings.TrimSpace(t) == "" {
			return nil
		}
		parts := strings.Split(t, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if s := strings.TrimSpace(p); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// ───────── 供接口层使用的同步包装 ─────────

// exportReportTo 把扫描结果写成 HTML 报告（Headless 可用，不弹对话框）。
func (a *App) exportReportTo(output string) (any, error) {
	a.mu.Lock()
	res := a.result
	a.mu.Unlock()
	if res == nil {
		return nil, fmt.Errorf("还没有扫描结果：先调用 disk.scan_start")
	}
	if strings.TrimSpace(output) == "" {
		output = "winclean-report.html"
	}
	abs, err := filepath.Abs(output)
	if err != nil {
		return nil, err
	}
	if err := report.WriteHTML(abs, res, 500); err != nil {
		return nil, err
	}
	if st, err := os.Stat(abs); err == nil {
		return map[string]any{"path": abs, "bytes": st.Size()}, nil
	}
	return map[string]any{"path": abs}, nil
}
