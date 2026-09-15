// Package javadev 提供「Java 开发」页的服务运行时：
// 按模块单独启动/停止进程、加载依赖、查看 Git 变更。
//
// 定位是「简化版 IDE 的运行栏」，不是 IDE：
//   - 只做进程编排与信息呈现，不碰源码，不做代码分析、索引、补全。
//   - 启动方式是「一条命令 + 一个工作目录」，用户在界面上看得见、改得动，
//     不需要为每个项目写一份插件式配置。
//   - 进程一律后台运行、不弹黑窗，输出实时流回界面（与打包模块同样的做法）。
package javadev

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// CREATE_NO_WINDOW：为子进程创建但不显示控制台窗口。
//
// 本程序是 GUI 子系统（-H windowsgui），没有控制台；这种进程启动控制台子进程
// 时 Windows 会【新建一个控制台窗口】——不加这个标志，启动服务就会弹出黑窗。
// 打包模块踩过同一个坑（见 internal/deploy/deploy.go），这里沿用同样的做法。
const createNoWindow = 0x08000000

// ansiRE 匹配终端颜色/控制序列，用于把 mvn / npm 输出洗成纯文本。
var ansiRE = regexp.MustCompile(
	`\x1b\[[0-9;?]*[ -/]*[@-~]` +
		`|\x1b\][^\x1b\x07]*(?:\x07|\x1b\\)` +
		`|\x1b[@-Z\\-_]`)

func stripANSI(s string) string {
	if !strings.ContainsRune(s, 0x1b) {
		return s
	}
	return ansiRE.ReplaceAllString(s, "")
}

// childEnv 构造子进程环境：注入 JDK 与 Maven 的 bin 目录。
//
// 为什么要显式注入 Maven bin：用户配置里存的是 mvn.cmd 的完整路径
// （D:\workplace\apache-maven-3.9.6\bin\mvn.cmd），而模块的启动命令里写的是
// 朴素的 `mvn`。把它的 bin 目录前置到 PATH，配置里的路径就能对命令生效，
// 用户也不必在每条命令里写绝对路径。
//
// 编码参数（MAVEN_OPTS/JAVA_TOOL_OPTIONS）与打包模块同理：
// 中文 Windows 上 Java 默认按 GBK 写输出，日志会乱码。
func childEnv(jdk, mavenPath string) []string {
	env := os.Environ()

	var prepend []string
	addDir := func(d string) {
		d = strings.TrimSpace(d)
		if d == "" {
			return
		}
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			prepend = append(prepend, d)
		}
	}

	if jdk != "" {
		addDir(filepath.Join(jdk, "bin"))
	}
	if mavenPath != "" {
		// 配置里存的是 mvn.cmd 全路径，取其所在目录
		if st, err := os.Stat(mavenPath); err == nil && !st.IsDir() {
			addDir(filepath.Dir(mavenPath))
		} else {
			addDir(mavenPath)
		}
	}

	out := make([]string, 0, len(env)+4)
	pathSet := false
	for _, kv := range env {
		up := strings.ToUpper(kv)
		switch {
		case jdk != "" && strings.HasPrefix(up, "JAVA_HOME="):
			continue // 稍后统一写入
		case strings.HasPrefix(up, "PATH="):
			out = append(out, "PATH="+joinPrepend(prepend, kv[len("PATH="):]))
			pathSet = true
		default:
			out = append(out, kv)
		}
	}
	if jdk != "" {
		out = append(out, "JAVA_HOME="+jdk)
	}
	if !pathSet && len(prepend) > 0 {
		out = append(out, "PATH="+strings.Join(prepend, string(os.PathListSeparator)))
	}
	if !hasEnvKey(out, "MAVEN_OPTS=") {
		out = append(out, "MAVEN_OPTS=-Dfile.encoding=UTF-8")
	}
	if !hasEnvKey(out, "JAVA_TOOL_OPTIONS=") {
		out = append(out, "JAVA_TOOL_OPTIONS=-Dfile.encoding=UTF-8")
	}
	return out
}

func joinPrepend(dirs []string, rest string) string {
	if len(dirs) == 0 {
		return rest
	}
	return strings.Join(dirs, string(os.PathListSeparator)) + string(os.PathListSeparator) + rest
}

func hasEnvKey(env []string, prefix string) bool {
	for _, kv := range env {
		if strings.HasPrefix(strings.ToUpper(kv), prefix) {
			return true
		}
	}
	return false
}

// procAttr 组装子进程启动属性：接管命令行 + 不建窗口。
func procAttr(cmdline string) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CmdLine:       "cmd.exe /c " + cmdline,
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
}

// killTree 终止进程及其所有子进程。
//
// 必须连子进程一起杀：`mvn spring-boot:run` 实际是
// cmd.exe → mvn.cmd → java.exe 三层，只杀最外层会留下 java 继续占着端口，
// 用户看到的现象是「服务停了但端口还被占」。
func killTree(pid int) {
	if pid <= 0 {
		return
	}
	c := exec.Command("taskkill", "/T", "/F", "/PID", fmt.Sprint(pid))
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	_ = c.Run()
}

// streamLines 把管道里的输出按行回调出去。
//
// 子进程可能用 \r 刷同一行（进度条、Spring 的 banner 之外的日志），
// 统一拆成多行再送界面，避免一行里堆着一堆覆盖过的内容。
func streamLines(r io.Reader, emit func(string)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := stripANSI(sc.Text())
		for _, l := range strings.Split(strings.ReplaceAll(line, "\r", "\n"), "\n") {
			l = strings.TrimRight(l, " \t")
			if strings.TrimSpace(l) == "" {
				continue
			}
			emit(l)
		}
	}
}

// waitExit 等待进程结束并把退出码回调出去。
func waitExit(ctx context.Context, cmd *exec.Cmd, done func(code int, err error)) {
	err := cmd.Wait()
	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	if err != nil && ctx.Err() != nil {
		// 主动取消（用户点了停止）：退出码没有意义，按 0 处理
		code = 0
	}
	done(code, err)
}

// waitForExit 轮询等待条件成立（用于「停止」保持同步语义）。
func waitForExit(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(120 * time.Millisecond)
	}
	return cond()
}

// mu 相关的辅助：把日志行并入环形缓冲。
func appendLogLine(logs []string, line string, limit int) []string {
	logs = append(logs, line)
	if limit > 0 && len(logs) > limit {
		logs = logs[len(logs)-limit:]
	}
	return logs
}
