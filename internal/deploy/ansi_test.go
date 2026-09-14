package deploy

import (
	"strings"
	"testing"
)

// TestStripANSI 验证终端控制序列被剥掉。
//
// 实测：vite 在管道输出下仍会写颜色码，日志窗格里会显示成
// "dist/[2massets/x.js[22m[36m 25.46 kB[39m" 这种噪声。
func TestStripANSI(t *testing.T) {
	cases := []struct{ in, want string }{
		{"普通日志行", "普通日志行"},
		{"\x1b[32m✓ built in 1m 45s\x1b[39m", "✓ built in 1m 45s"},
		{"\x1b[2mdist/\x1b[22m\x1b[36massets/a.js\x1b[39m  25.46 kB", "dist/assets/a.js  25.46 kB"},
		{"WARN  \x1b[33m", "WARN  "},
		{"\x1b]0;标题\x07内容", "内容"},
		{"", ""},
	}
	for _, c := range cases {
		if got := stripANSI(c.in); got != c.want {
			t.Errorf("stripANSI(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestStripANSIPreservesContent 确保不会误伤正常内容。
//
// 日志里会出现路径、方括号、数字，这些都不能被当成转义序列吃掉。
func TestStripANSIPreservesContent(t *testing.T) {
	for _, s := range []string{
		`D:\workplace\jdk17\bin\java.exe`,
		`[INFO] BUILD SUCCESS`,
		`已压缩 334 个文件，4.5 MB`,
		`[0-9]+ 这样的正则字面量`,
		"包含 esc 但不是转义: ESC 键",
	} {
		if got := stripANSI(s); got != s {
			t.Errorf("不应改动 %q，实际变成 %q", s, got)
		}
	}
	// 含真实转义符的才应被处理
	if got := stripANSI("\x1b[31m红字\x1b[0m"); strings.ContainsRune(got, 0x1b) {
		t.Error("应剥掉转义符")
	}
}
