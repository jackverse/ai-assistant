package deploy

import (
	"archive/zip"
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ───────── 配置 ─────────

// ModuleConfig 是一个待打包模块。
type ModuleConfig struct {
	Name string `yaml:"name" json:"name"`
	// Type 为 backend（默认）或 frontend
	Type string `yaml:"type" json:"type"`
	// Root 是相对【项目根】的模块目录；留空则等于 Name
	Root string `yaml:"root" json:"root"`
	// Source 是 frontend 的产物目录，默认 dist
	Source string `yaml:"source" json:"source"`
	// Script 是 frontend 的 npm script 名（可能含空格，如 "admin build:prod"）
	Script string `yaml:"script" json:"script"`
	// Output 是产物名，默认取 Name
	Output string `yaml:"output" json:"output"`
}

// ProjectConfig 是一个项目及其模块。
type ProjectConfig struct {
	Name    string         `yaml:"name" json:"name"`
	Root    string         `yaml:"root" json:"root"` // 相对 BasePath
	Modules []ModuleConfig `yaml:"modules" json:"modules"`
}

// Config 是部署打包模块的配置（持久化在助手 config.yaml 的 deploy 节）。
type Config struct {
	// BasePath 是代码根目录，项目 Root 相对它解析
	BasePath string `yaml:"base_path" json:"base_path"`
	// OutputDir 是产物输出目录（界面可选择）
	OutputDir string `yaml:"output_dir" json:"output_dir"`
	// MavenPath 留空表示使用自动探测到的 Maven
	MavenPath string `yaml:"maven_path" json:"maven_path"`
	// JDKPath 留空表示使用自动探测到的 JDK
	JDKPath string `yaml:"jdk_path" json:"jdk_path"`
	// KeepEnv 是环境过滤要保留的环境名，默认 prod
	KeepEnv string `yaml:"keep_env" json:"keep_env"`
	// SkipTests 是否跳过测试，默认 true（部署打包通常不需要跑测试）
	SkipTests *bool `yaml:"skip_tests" json:"skip_tests"`
	Projects  []ProjectConfig `yaml:"projects" json:"projects"`
}

// effectiveKeepEnv 返回实际使用的环境名。
func (c Config) effectiveKeepEnv() string {
	if strings.TrimSpace(c.KeepEnv) == "" {
		return "prod"
	}
	return strings.TrimSpace(c.KeepEnv)
}

func (c Config) skipTests() bool {
	// 默认 false = 与既有工具（bundle-packer 跑裸 `mvn clean package`）行为一致。
	// 改默认值属于行为变化，必须由用户在配置里显式打开，不能悄悄跳过测试。
	if c.SkipTests == nil {
		return false
	}
	return *c.SkipTests
}

// ───────── 事件 ─────────

// Event 是打包过程中推给界面的事件。
type Event struct {
	Kind    string   `json:"kind"` // state | log | done
	Phase   string   `json:"phase,omitempty"`
	Project string   `json:"project,omitempty"`
	Module  string   `json:"module,omitempty"`
	Line    string   `json:"line,omitempty"`
	Percent int      `json:"percent,omitempty"`
	Err     string   `json:"error,omitempty"`
	Outputs []string `json:"outputs,omitempty"`
}

// RunOptions 是本次打包的选择。
type RunOptions struct {
	Project   string   // 为空表示打包配置里的所有项目
	Modules   []string // 为空表示该项目下的所有模块
	OutputDir string   // 为空表示用配置里的 OutputDir
}

// ───────── 执行 ─────────

// Run 执行打包。事件通过 emit 回调推送（调用方负责转发给界面）。
//
// 打包在独立进程（mvn / npm）中完成，本函数只负责编排与文件整理。
// ctx 取消时会终止整个子进程树——Windows 上只杀父进程会留下 mvn 派生的
// java 进程继续跑，必须用 taskkill /T。
func Run(ctx context.Context, cfg Config, opts RunOptions, emit func(Event)) error {
	if emit == nil {
		emit = func(Event) {}
	}

	base := strings.TrimSpace(cfg.BasePath)
	if base == "" {
		return fmt.Errorf("未配置代码根目录（base_path）")
	}
	if !dirExists(base) {
		return fmt.Errorf("代码根目录不存在: %s", base)
	}
	outDir := strings.TrimSpace(opts.OutputDir)
	if outDir == "" {
		outDir = strings.TrimSpace(cfg.OutputDir)
	}
	if outDir == "" {
		return fmt.Errorf("未选择产物输出目录")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("创建输出目录失败: %w", err)
	}

	// 工具链：配置里的优先，否则用自动探测结果（Maven 取排序后第一个有效项）
	mvn := strings.TrimSpace(cfg.MavenPath)
	if mvn == "" {
		if d := Detect(nil); len(d.Mavens) > 0 && d.Mavens[0].Valid {
			mvn = d.Mavens[0].Path
		}
	}
	jdk := strings.TrimSpace(cfg.JDKPath)
	if jdk == "" {
		if d := Detect(nil); len(d.JDKs) > 0 && d.JDKs[0].Valid {
			jdk = d.JDKs[0].Path
		}
	}
	if jdk == "" {
		return fmt.Errorf("未找到可用的 JDK。请安装 JDK 或在界面里手动指定")
	}
	if !isFile(filepath.Join(jdk, "bin", "javac.exe")) {
		return fmt.Errorf("指定的 JDK 无效（缺 bin\\javac.exe）: %s", jdk)
	}

	emit(Event{Kind: "state", Phase: "准备",
		Line: fmt.Sprintf("JDK: %s\nMaven: %s\n输出目录: %s", jdk, orNone(mvn), outDir)})

	type job struct {
		proj ProjectConfig
		mod  ModuleConfig
	}
	var jobs []job
	for _, p := range cfg.Projects {
		if opts.Project != "" && p.Name != opts.Project {
			continue
		}
		want := map[string]bool{}
		for _, m := range opts.Modules {
			want[m] = true
		}
		for _, m := range p.Modules {
			if len(want) > 0 && !want[m.Name] {
				continue
			}
			jobs = append(jobs, job{proj: p, mod: m})
		}
	}
	if len(jobs) == 0 {
		return fmt.Errorf("没有匹配的模块（检查配置里的项目名与模块名）")
	}

	emit(Event{Kind: "state", Phase: "开始", Percent: 0,
		Line: fmt.Sprintf("共 %d 个模块待打包", len(jobs))})

	var (
		outputs []string
		failed  []string
	)
	for i, j := range jobs {
		select {
		case <-ctx.Done():
			emit(Event{Kind: "log", Line: "已取消"})
			return ctx.Err()
		default:
		}

		pct := i * 100 / len(jobs)
		emit(Event{Kind: "state", Phase: "打包", Project: j.proj.Name,
			Module: j.mod.Name, Percent: pct})

		mod := j.mod
		if mod.Type == "" {
			mod.Type = "backend"
		}
		if mod.Output == "" {
			mod.Output = mod.Name
		}
		if mod.Root == "" {
			mod.Root = mod.Name
		}

		modDir := filepath.Join(base, j.proj.Root, mod.Root)
		var err error
		var out string
		switch mod.Type {
		case "backend":
			out, err = packBackend(ctx, cfg, modDir, mod, outDir, mvn, jdk, emit)
		case "frontend":
			out, err = packFrontend(ctx, cfg, modDir, mod, outDir, emit)
		default:
			err = fmt.Errorf("不支持的模块类型 %q（应为 backend 或 frontend）", mod.Type)
		}

		if err != nil {
			failed = append(failed, j.proj.Name+"/"+mod.Name)
			emit(Event{Kind: "log", Project: j.proj.Name, Module: mod.Name,
				Line: "失败: " + err.Error()})
			continue
		}
		outputs = append(outputs, out)
		emit(Event{Kind: "log", Project: j.proj.Name, Module: mod.Name,
			Line: "完成: " + out})
	}

	emit(Event{Kind: "state", Phase: "完成", Percent: 100})
	emit(Event{Kind: "done", Outputs: outputs,
		Err:     strings.Join(failed, ", "),
		Line:    fmt.Sprintf("成功=%d 失败=%d", len(outputs), len(failed))})

	if len(failed) > 0 {
		return fmt.Errorf("有 %d 个模块打包失败: %s", len(failed), strings.Join(failed, ", "))
	}
	return nil
}

// packBackend 打包一个后端模块：mvn clean package → 解出 war 结构 → 环境过滤 → zip。
func packBackend(ctx context.Context, cfg Config, modDir string, mod ModuleConfig,
	outDir, mvn, jdk string, emit func(Event)) (string, error) {

	emit(Event{Kind: "log", Module: mod.Name, Line: "模块目录: " + modDir})
	if !isFile(filepath.Join(modDir, "pom.xml")) {
		return "", fmt.Errorf("模块目录下没有 pom.xml（后端模块必须指向含 pom.xml 的目录）: %s", modDir)
	}
	if mvn == "" {
		return "", fmt.Errorf("未找到可用的 Maven。请安装 Maven 或在界面里手动指定")
	}

	if err := runStep(ctx, modDir, mod.Name, jdk, mvnLabel(mvn), mvnArgs(mvn, cfg.skipTests()), emit); err != nil {
		return "", err
	}

	// 取产物：优先解压 maven 产出的 war（lib 完整性由构建保证，比手工拼装可靠）
	assembly, cleanup, err := prepareAssembly(modDir, mod.Name, emit)
	if err != nil {
		return "", err
	}
	defer cleanup()

	removed, err := filterEnvFiles(assembly, cfg.effectiveKeepEnv())
	if err != nil {
		return "", err
	}
	emit(Event{Kind: "log", Module: mod.Name,
		Line: fmt.Sprintf("环境过滤：保留 %s，移除 %d 个非 %s 配置文件",
			cfg.effectiveKeepEnv(), removed, cfg.effectiveKeepEnv())})

	zipPath := filepath.Join(outDir, mod.Output+"_war_exploded.zip")
	if err := zipDir(assembly, zipPath, mod.Name, emit); err != nil {
		return "", err
	}
	return zipPath, nil
}

// packFrontend 打包一个前端模块：npm run <script> → 压缩产物目录。
func packFrontend(ctx context.Context, cfg Config, modDir string, mod ModuleConfig,
	outDir string, emit func(Event)) (string, error) {

	emit(Event{Kind: "log", Module: mod.Name, Line: "模块目录: " + modDir})
	if !isFile(filepath.Join(modDir, "package.json")) {
		return "", fmt.Errorf("模块目录下没有 package.json: %s", modDir)
	}
	if strings.TrimSpace(mod.Script) == "" {
		return "", fmt.Errorf("前端模块未配置 script（package.json 里的脚本名）")
	}

	npm, err := exec.LookPath("npm.cmd")
	if err != nil {
		if npm, err = exec.LookPath("npm"); err != nil {
			return "", fmt.Errorf("未找到 npm，请确认 Node.js 已安装且在 PATH 中")
		}
	}
	if err := runStep(ctx, modDir, mod.Name, "", npm, []string{"run", mod.Script}, emit); err != nil {
		return "", err
	}

	src := mod.Source
	if strings.TrimSpace(src) == "" {
		src = "dist"
	}
	srcDir := filepath.Join(modDir, src)
	if !dirExists(srcDir) {
		return "", fmt.Errorf("构建产物目录不存在: %s（构建脚本是否真的产出了 %s？）", srcDir, src)
	}

	zipPath := filepath.Join(outDir, mod.Output+".zip")
	if err := zipDir(srcDir, zipPath, mod.Name, emit); err != nil {
		return "", err
	}
	return zipPath, nil
}

// mvnLabel 返回用于日志展示的 Maven 名称。
func mvnLabel(mvn string) string {
	base := filepath.Base(mvn)
	if base == "" {
		return mvn
	}
	return base
}

// mvnArgs 组装 mvn 参数。mvnw.cmd 与 mvn.cmd 的参数一致。
func mvnArgs(mvn string, skipTests bool) []string {
	args := []string{"clean", "package"}
	if skipTests {
		args = append(args, "-DskipTests")
	}
	return args
}

// runStep 执行一个外部命令并把输出实时回调出去。
//
// 三个踩过的坑都体现在这里：
//
//  1. 参数必须【分开传】给 exec.Command，不能自己拼好整条命令串再传：
//     Go 在 Windows 上会对每个参数再做一次转义，把我拼好的引号变成 \"，
//     结果 cmd.exe 收到 `\"C:\Program Files\...\npm.cmd\"` 直接报
//     「不是内部或外部命令」。日志里展示的命令行是单独拼的（见 buildCmdLine）。
//
//  2. .cmd/.bat 必须经 cmd.exe 执行（CreateProcess 不能直接跑批处理）。
//
//  3. 中文 Windows 上 mvn/npm 默认按 GBK 输出，直接当 UTF-8 读会乱码
//     （实测日志出现「杈撳嚭鐩�褰�」）。用 chcp 65001 把子进程控制台切到
//     UTF-8，并给 Java 系工具加 -Dfile.encoding=UTF-8。
//
// 环境变量：把选定 JDK 的 JAVA_HOME 传进去并前置其 bin——
// 否则 mvn 可能用到系统里另一个 JDK，导致「明明选了 JDK 17 却按 JDK 8 编译」。
func runStep(ctx context.Context, dir, module, jdk, exe string, args []string, emit func(Event)) error {
	if !fileExistsAny(exe) {
		return fmt.Errorf("可执行文件不存在: %s", exe)
	}

	// 日志里给用户看的是人可读的完整命令（带引号）
	cmdline := buildCmdLine(exe, args)
	emit(Event{Kind: "log", Module: module,
		Line: "$ " + cmdline + "  (cwd=" + dir + ")"})

	// 真正执行时用 SysProcAttr.CmdLine 直接接管命令行。
	//
	// 为什么要绕过 Go 的参数转义：
	//   - cmd.exe 的 /c 只接受【一整条命令字符串】。若把 "chcp 65001 >nul &&"
	//     作为独立参数传入，cmd 会把它当成命令名 → 「文件名、目录名或卷标语法不正确」。
	//   - 若把整条串作为单个参数传入，Go 会对其再做一次转义，把引号变成 \"，
	//     cmd.exe 不认这种转义 → 「'\"C:\Program Files\...' 不是内部或外部命令」。
	//   - 两个坑都实测踩过。CmdLine 让我们完全控制命令行文本，由我们自己保证引号正确。
	full := "chcp 65001 >nul && " + cmdline
	cmd := exec.CommandContext(ctx, "cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: "cmd.exe /c " + full}
	cmd.Dir = dir
	cmd.Env = buildEnv(jdk)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动失败: %w", err)
	}

	var wg sync.WaitGroup
	stream := func(r io.Reader) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			emit(Event{Kind: "log", Module: module, Line: sc.Text()})
		}
	}
	wg.Add(2)
	go stream(stdout)
	go stream(stderr)

	// 取消时终止整棵进程树：只杀 cmd.exe 会留下 mvn 派生的 java 进程
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			killTree(cmd.Process.Pid)
		case <-done:
		}
	}()

	err = cmd.Wait()
	close(done)
	wg.Wait()

	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("已取消")
		}
		return fmt.Errorf("命令失败: %w", err)
	}
	return nil
}

// buildCmdLine 组装命令行并对含空格的参数加引号。
//
// 必须自己做而不是依赖 Go 的默认拼接：npm script 名可能含空格
// （本机实测 wic-admin-web 的 script 就叫 "admin build:prod"），
// 不加引号会被拆成两个参数。
func buildCmdLine(exe string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, quoteIfNeeded(exe))
	for _, a := range args {
		parts = append(parts, quoteIfNeeded(a))
	}
	return strings.Join(parts, " ")
}

func quoteIfNeeded(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, " \t&()[]{}^=;!'+,`~") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}

// buildEnv 构造子进程环境：注入 JAVA_HOME 并把其 bin 前置到 PATH。
//
// 同时设置 MAVEN_OPTS 与 JAVA_TOOL_OPTIONS 的编码参数：
// 中文 Windows 上 Java 默认按 GBK 写输出，会导致界面上的构建日志乱码。
func buildEnv(jdk string) []string {
	env := os.Environ()
	if jdk == "" {
		return withEncodingOpts(env)
	}
	binDir := filepath.Join(jdk, "bin")

	out := make([]string, 0, len(env)+2)
	pathSet := false
	for _, kv := range env {
		up := strings.ToUpper(kv)
		switch {
		case strings.HasPrefix(up, "JAVA_HOME="):
			continue // 稍后统一写入
		case strings.HasPrefix(up, "PATH="):
			out = append(out, "PATH="+binDir+string(os.PathListSeparator)+kv[len("PATH="):])
			pathSet = true
		default:
			out = append(out, kv)
		}
	}
	out = append(out, "JAVA_HOME="+jdk)
	if !pathSet {
		out = append(out, "PATH="+binDir)
	}
	return withEncodingOpts(out)
}

// withEncodingOpts 追加 UTF-8 编码选项，避免构建日志在中文 Windows 上乱码。
func withEncodingOpts(env []string) []string {
	const (
		mavenOpts = "-Dfile.encoding=UTF-8"
	)
	hasMavenOpts, hasToolOpts := false, false
	for _, kv := range env {
		up := strings.ToUpper(kv)
		if strings.HasPrefix(up, "MAVEN_OPTS=") {
			hasMavenOpts = true
		}
		if strings.HasPrefix(up, "JAVA_TOOL_OPTIONS=") {
			hasToolOpts = true
		}
	}
	if !hasMavenOpts {
		env = append(env, "MAVEN_OPTS="+mavenOpts)
	}
	if !hasToolOpts {
		env = append(env, "JAVA_TOOL_OPTIONS=-Dfile.encoding=UTF-8")
	}
	return env
}

// killTree 终止进程及其所有子进程。
func killTree(pid int) {
	if pid <= 0 {
		return
	}
	c := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	_ = c.Run()
}

// ───────── 组装 war 结构 ─────────

// prepareAssembly 准备待压缩的目录，返回该目录与清理函数。
//
// 优先解压 target 下的 war——war 是 maven 的权威产物，WEB-INF/lib 的
// 完整性由构建保证，比手工拼 classes+lib 可靠。没有 war 时退化为
// 使用 target/classes 并明确告警（部署侧可能缺依赖 jar）。
func prepareAssembly(modDir, module string, emit func(Event)) (string, func(), error) {
	target := filepath.Join(modDir, "target")
	wars, _ := filepath.Glob(filepath.Join(target, "*.war"))

	if len(wars) > 0 {
		war := wars[0]
		// 多个 war 时取第一个，但要说清楚，避免"打错了包"
		if len(wars) > 1 {
			emit(Event{Kind: "log", Module: module,
				Line: fmt.Sprintf("发现 %d 个 war，使用 %s", len(wars), filepath.Base(war))})
		}
		dest, err := os.MkdirTemp("", "wc-assembly-*")
		if err != nil {
			return "", func() {}, err
		}
		if err := unzipTo(war, dest); err != nil {
			os.RemoveAll(dest)
			return "", func() {}, fmt.Errorf("解压 war 失败: %w", err)
		}
		emit(Event{Kind: "log", Module: module,
			Line: "已解压 " + filepath.Base(war) + " 作为打包内容"})
		return dest, func() { os.RemoveAll(dest) }, nil
	}

	classes := filepath.Join(target, "classes")
	if dirExists(classes) {
		dest, err := os.MkdirTemp("", "wc-assembly-*")
		if err != nil {
			return "", func() {}, err
		}
		if err := copyDir(classes, filepath.Join(dest, "WEB-INF", "classes")); err != nil {
			os.RemoveAll(dest)
			return "", func() {}, err
		}
		emit(Event{Kind: "log", Module: module,
			Line: "⚠️ 未找到 war，退化为仅打包 target/classes；" +
				"WEB-INF/lib 缺失，部署前请自行确认依赖 jar 完整"})
		return dest, func() { os.RemoveAll(dest) }, nil
	}

	return "", func() {}, fmt.Errorf(
		"未找到构建产物：%s 下既没有 *.war 也没有 classes 目录（构建是否成功？）", target)
}

// filterEnvFiles 删除组装目录里非目标环境的 Spring 配置文件。
//
// ⚠️ 关键安全约束：只操作【组装目录】（从 war 解压出来的临时目录），
// 绝不动源码目录里的配置文件。误删源码里的 application-prod.yml
// 是不可接受的损失。
//
// 规则：仅处理 WEB-INF/classes 下的 application-*.yml（含 yaml），
// 保留 application.yml 与文件名含 "-<keepEnv>" 的那些
// （如 application-prod.yml、application-common-prod.yml）。
func filterEnvFiles(assemblyDir, keepEnv string) (int, error) {
	classesDir := filepath.Join(assemblyDir, "WEB-INF", "classes")
	if !dirExists(classesDir) {
		return 0, nil
	}

	entries, err := os.ReadDir(classesDir)
	if err != nil {
		return 0, err
	}

	removed := 0
	keepSuffix := "-" + keepEnv
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, "application-") {
			continue
		}
		if !strings.HasSuffix(lower, ".yml") && !strings.HasSuffix(lower, ".yaml") {
			continue
		}
		// application-prod.yml / application-common-prod.yml 保留
		base := strings.TrimSuffix(strings.TrimSuffix(lower, ".yaml"), ".yml")
		if strings.HasSuffix(base, keepSuffix) {
			continue
		}
		if err := os.Remove(filepath.Join(classesDir, name)); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// ───────── zip / unzip ─────────

// zipDir 把目录压缩为 zip，并定期回报进度（大目录时压缩要几秒）。
func zipDir(srcDir, zipPath, module string, emit func(Event)) error {
	files, err := collectFiles(srcDir)
	if err != nil {
		return err
	}

	out, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	defer out.Close()

	zw := zip.NewWriter(out)
	start := time.Now()
	for i, rel := range files {
		full := filepath.Join(srcDir, rel)
		if err := addFileToZip(zw, full, filepath.ToSlash(rel)); err != nil {
			zw.Close()
			return fmt.Errorf("压缩 %s 失败: %w", rel, err)
		}
		if i%500 == 0 && i > 0 {
			emit(Event{Kind: "log", Module: module,
				Line: fmt.Sprintf("已压缩 %d/%d 个文件…", i, len(files))})
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}

	var size int64
	if st, err := os.Stat(zipPath); err == nil {
		size = st.Size()
	}
	emit(Event{Kind: "log", Module: module,
		Line: fmt.Sprintf("已压缩 %d 个文件，%.1f MB，耗时 %s",
			len(files), float64(size)/1024/1024, time.Since(start).Round(time.Millisecond))})
	return nil
}

func addFileToZip(zw *zip.Writer, fullPath, nameInZip string) error {
	st, err := os.Stat(fullPath)
	if err != nil {
		return err
	}
	hdr, err := zip.FileInfoHeader(st)
	if err != nil {
		return err
	}
	hdr.Name = nameInZip
	hdr.Method = zip.Deflate

	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	f, err := os.Open(fullPath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

// collectFiles 递归收集目录下所有文件（相对路径，正斜杠分隔）。
func collectFiles(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out = append(out, rel)
		return nil
	})
	return out, err
}

// unzipTo 解压 zip 到目标目录。
func unzipTo(zipPath, destDir string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()

	for _, f := range zr.File {
		target := filepath.Join(destDir, filepath.FromSlash(f.Name))
		// 防目录穿越：zip 条目里可能含 ../
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(destDir)) {
			return fmt.Errorf("zip 条目路径越界: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		w, err := os.Create(target)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(w, rc)
		rc.Close()
		w.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// copyDir 递归复制目录。
func copyDir(src, dst string) error {
	files, err := collectFiles(src)
	if err != nil {
		return err
	}
	for _, rel := range files {
		s := filepath.Join(src, rel)
		d := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(d), 0o755); err != nil {
			return err
		}
		if err := copyFile(s, d); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(自动探测)"
	}
	return s
}

// fileExistsAny 支持传目录（例如某些环境下 mvn 是脚本而非 .cmd）。
func fileExistsAny(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
