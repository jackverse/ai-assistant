package deploy

import "testing"

// TestDetectOnThisMachine 验证工具链探测在本机的实际效果。
//
// 不用断言「必须找到」——CI 或他人机器上可能确实没装 JDK/Maven，
// 那样断言会误报。这里断言的是「找到的东西都必须是有效的」，
// 并把结果打印出来供人工核对。
func TestDetectOnThisMachine(t *testing.T) {
	res := Detect([]string{`D:\code`})

	t.Logf("JAVA_HOME=%q  MAVEN_HOME/M2_HOME=%q", res.EnvJAVA, res.EnvMvn)
	for _, n := range res.Notes {
		t.Logf("提示: %s", n)
	}

	t.Logf("探测到 %d 个 JDK：", len(res.JDKs))
	for _, j := range res.JDKs {
		t.Logf("  %-14s %-28s [%s] %s", j.Version, j.Path, j.Source, j.Detail)
	}
	t.Logf("探测到 %d 个 Maven：", len(res.Mavens))
	for _, m := range res.Mavens {
		t.Logf("  %-10s %-46s [%s]", m.Version, m.Path, m.Source)
	}

	// 有效性断言：验证过的候选必须真的存在对应可执行文件
	for _, j := range res.JDKs {
		if !j.Valid {
			continue
		}
		if !isFile(j.Path + `\bin\javac.exe`) {
			t.Errorf("JDK 候选标记为有效但 javac 不存在: %s", j.Path)
		}
		if j.Version == "" {
			t.Errorf("JDK %s 未读到版本（release 文件缺失或格式变化）", j.Path)
		}
	}
	for _, m := range res.Mavens {
		if !m.Valid {
			continue
		}
		if !isFile(m.Path) {
			t.Errorf("Maven 候选标记为有效但 mvn.cmd 不存在: %s", m.Path)
		}
	}

	// 重复项检查（同一路径不应被多条规则重复加入）
	seen := map[string]int{}
	for _, j := range res.JDKs {
		seen[j.Path]++
	}
	for p, n := range seen {
		if n > 1 {
			t.Errorf("JDK 路径重复 %d 次: %s", n, p)
		}
	}
}
