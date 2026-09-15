package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"winclean/internal/ai"
	"winclean/internal/config"
	"winclean/internal/scan"
)

// TestAIAnalysisPipeline 端到端验证「扫描 → 摘要 → AI 分析」整条链路。
//
// 用一个小目录做真实扫描（不是合成数据），构建摘要后调用真实配置的
// 大模型网关，输出分析结论。默认跳过，显式开启：
//
//	WC_REAL_CFG=1 go test . -run TestAIAnalysisPipeline -v
//
// 这个用例覆盖了 GUI 之外的全部环节；界面只负责展示流式结果。
func TestAIAnalysisPipeline(t *testing.T) {
	if os.Getenv("WC_REAL_CFG") != "1" {
		t.Skip("未设置 WC_REAL_CFG=1，跳过")
	}

	cfg, _, err := config.Load()
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	s := ai.Settings{BaseURL: cfg.AI.BaseURL, APIKey: cfg.AI.APIKey, Model: cfg.AI.Model}
	if !s.Valid() {
		t.Skip("AI 未配置，跳过")
	}

	// 用 deploy 模块的 base_path 下选一个小项目做真实扫描
	// （wic-sh 后端模块目录，几万文件级别，扫描秒级）
	scanRoot := scanSampleDir()
	if scanRoot == "" {
		t.Skip("找不到可扫描的目录")
	}

	t.Logf("扫描目录: %s", scanRoot)
	scanCtx, scanCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	res, err := scan.Run(scanCtx, scan.Options{
		Roots: []string{scanRoot}, TopFiles: 20, MinSize: 1 << 20,
	})
	scanCancel()
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	t.Logf("扫描完成: %d 文件 / %d 目录 / %s",
		res.Stats.FilesScanned, res.Stats.DirsScanned,
		fmtByteGB(res.Stats.BytesOnDisk))

	inUse := probeTopFiles(res.LargestFiles, 20)
	digest := buildScanDigest(res, inUse)
	if strings.TrimSpace(digest) == "" {
		t.Fatal("digest 为空")
	}
	t.Logf("摘要 %d 字节", len(digest))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var deltas int
	start := time.Now()
	reply, err := ai.Stream(ctx, s, []ai.Message{
		{Role: "system", Content: scanAISysPrompt},
		{Role: "user", Content: "扫描事实：\n" + digest + "\n\n请按规则输出分析。"},
	}, func(string) { deltas++ })
	if err != nil {
		t.Fatalf("AI 分析失败: %v", err)
	}

	t.Logf("AI 耗时 %s，%d 个增量片段", time.Since(start).Round(time.Millisecond), deltas)
	t.Logf("分析结论:\n%s", reply)

	if strings.TrimSpace(reply) == "" {
		t.Error("AI 分析为空")
	}
	for _, section := range []string{"建议删除", "不要删除"} {
		if !strings.Contains(reply, section) {
			t.Errorf("分析结论缺少【%s】分节", section)
		}
	}
}

// filepath_ 找一个小目录做扫描样本。
func scanSampleDir() string {
	for _, c := range []string{
		`D:\code\wic-sh\wic-database`,
		`D:\code\wic-sh\wic-common`,
	} {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	return ""
}

func fmtByteGB(b int64) string {
	return fmt.Sprintf("%.2f GB", float64(b)/(1<<30))
}
