package deploy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// 开发态信息探测：这个模块该怎么启动、它用哪个端口。
//
// 为什么值得单独做一层：用户在「Java 开发」页看到的应该是可直接点启动的服务，
// 而不是一张空白表单让他手抄 "mvn spring-boot:run" 和端口号。
// 探测全部基于模块目录里的既有文件（pom.xml / package.json / application*.yml /
// vite.config.*），只读取，不修改。

// DevHint 是一次探测的结果。
type DevHint struct {
	Run   string `json:"run"`   // 启动命令；空表示没认出来，需要用户手填
	Port  int    `json:"port"`  // 期望端口；0 表示没认出来
	Note  string `json:"note"`  // 给用户看的说明
	Found bool   `json:"found"` // 是否认出了启动命令
}

// DetectDev 推断模块的启动命令与端口。
func DetectDev(modDir, modType string) DevHint {
	if !dirExists(modDir) {
		return DevHint{Note: "模块目录不存在: " + modDir}
	}
	if modType == "frontend" {
		return detectFrontendDev(modDir)
	}
	return detectBackendDev(modDir)
}

// ───────── 后端（Maven / Spring Boot） ─────────

var springBootPluginRE = regexp.MustCompile(`spring-boot-maven-plugin`)

func detectBackendDev(modDir string) DevHint {
	pom := filepath.Join(modDir, "pom.xml")
	data, err := os.ReadFile(pom)
	if err != nil {
		return DevHint{Note: "读不到 pom.xml，无法判断启动方式"}
	}

	h := DevHint{}
	if springBootPluginRE.Match(data) {
		h.Run = "mvn spring-boot:run"
		h.Found = true
	} else {
		h.Note = "pom.xml 里没有 spring-boot-maven-plugin，未认出启动方式"
	}

	// 端口：优先用 application*.yml 里 server.port
	port, src := readSpringPort(filepath.Join(modDir, "src", "main", "resources"))
	if port > 0 {
		h.Port = port
		if h.Note != "" {
			h.Note += "；"
		}
		h.Note += "端口取自 " + src
	} else if h.Found {
		if h.Note != "" {
			h.Note += "；"
		}
		h.Note += "未在 application*.yml 里找到 server.port，请手填端口以便检测占用"
	}
	return h
}

// readSpringPort 从 src/main/resources 下的 application 配置里取 server.port。
//
// 之所以自己扫 YAML 而不是上解析库：这里只需要一个键，
// 且必须容忍中文注释、占位符、多 profile 文件等各种写法；
// 全量反序列化反而会因为这些写法直接失败。
// 优先级：application.yml 里 spring.profiles.active 指向的文件（开发时真正生效的那个），
// 其次 application-dev.yml，再次 application.yml，最后任意 application-*.yml。
func readSpringPort(resDir string) (int, string) {
	if !dirExists(resDir) {
		return 0, ""
	}
	files, _ := filepath.Glob(filepath.Join(resDir, "application*.yml"))
	files2, _ := filepath.Glob(filepath.Join(resDir, "application*.yaml"))
	files = append(files, files2...)
	if len(files) == 0 {
		// .properties 写法：server.port=8080
		props, _ := filepath.Glob(filepath.Join(resDir, "application*.properties"))
		for _, p := range props {
			if v := readPropertyPort(p); v > 0 {
				return v, filepath.Base(p)
			}
		}
		return 0, ""
	}

	byBase := map[string]string{}
	for _, f := range files {
		byBase[strings.ToLower(filepath.Base(f))] = f
	}

	order := []string{}
	if active := readActiveProfile(byBase["application.yml"]); active != "" {
		// active 可能是 dev，也可能是 dev,test 这种组合
		for _, a := range strings.Split(active, ",") {
			a = strings.TrimSpace(strings.ToLower(a))
			if a != "" {
				order = append(order, "application-"+a+".yml")
			}
		}
	}
	order = append(order, "application-dev.yml", "application.yml")
	// 兜底：其余 application-*.yml 按文件名顺序
	for _, f := range files {
		order = append(order, strings.ToLower(filepath.Base(f)))
	}

	seen := map[string]bool{}
	for _, name := range order {
		if seen[name] {
			continue
		}
		seen[name] = true
		f, ok := byBase[name]
		if !ok {
			continue
		}
		if v := readServerPortInYAML(f); v > 0 {
			return v, filepath.Base(f)
		}
	}
	return 0, ""
}

// readServerPortInYAML 在 YAML 里找 server: 块下的 port:。
//
// 不能直接全局搜 "port:"：spring.redis.port、management.server.port
// 之类的键会先被撞上（实测 application-dev.yml 里就有多处 port）。
// 所以按缩进界定 server 块的范围，只在块内找 port。
func readServerPortInYAML(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")

	inServer := false
	serverIndent := 0
	for _, raw := range lines {
		if strings.TrimSpace(raw) == "" || strings.HasPrefix(strings.TrimSpace(raw), "#") {
			continue
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " \t"))
		trimmed := strings.TrimSpace(raw)

		if inServer && indent <= serverIndent {
			inServer = false // 缩进退回，server 块结束
		}
		if !inServer {
			if key, _, ok := splitYAMLKey(trimmed); ok && strings.EqualFold(key, "server") {
				inServer = true
				serverIndent = indent
			}
			continue
		}
		key, val, ok := splitYAMLKey(trimmed)
		if !ok || !strings.EqualFold(key, "port") {
			continue
		}
		if n, err := strconv.Atoi(strings.Trim(val, `"'`)); err == nil && n > 0 && n < 65536 {
			return n
		}
	}
	return 0
}

// splitYAMLKey 拆 "key: value"，值可为空（块起始行）。
func splitYAMLKey(s string) (key, val string, ok bool) {
	i := strings.Index(s, ":")
	if i <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(s[:i])
	val = strings.TrimSpace(s[i+1:])
	if key == "" || strings.ContainsAny(key, "{}[]") {
		return "", "", false
	}
	return key, val, true
}

// readActiveProfile 取 spring.profiles.active 的值。
func readActiveProfile(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	re := regexp.MustCompile(`(?m)^\s*active:\s*([^\s#]+)`)
	if m := re.FindSubmatch(data); len(m) == 2 {
		return strings.Trim(string(m[1]), `"'`)
	}
	return ""
}

func readPropertyPort(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	re := regexp.MustCompile(`(?m)^\s*server\.port\s*[:=]\s*(\d+)`)
	if m := re.FindSubmatch(data); len(m) == 2 {
		if n, err := strconv.Atoi(string(m[1])); err == nil {
			return n
		}
	}
	return 0
}

// ───────── 前端（npm / vite） ─────────

func detectFrontendDev(modDir string) DevHint {
	h := DevHint{}

	pkgPath := filepath.Join(modDir, "package.json")
	data, err := os.ReadFile(pkgPath)
	if err != nil {
		h.Note = "读不到 package.json，无法判断启动方式"
		return h
	}
	var pkg packageJSON
	if err := json.Unmarshal(data, &pkg); err != nil {
		h.Note = "package.json 无法解析"
		return h
	}

	if script := pickDevScript(pkg.Scripts); script != "" {
		// 脚本名可能含空格（本机实测就叫 "admin dev"），必须加引号
		h.Run = "npm run " + quoteIfNeeded(script)
		h.Found = true
	} else {
		h.Note = "package.json 里没有 dev / serve 脚本"
	}

	if port, src := readFrontendPort(modDir); port > 0 {
		h.Port = port
		if h.Note != "" {
			h.Note += "；"
		}
		h.Note += "端口取自 " + src
	}
	return h
}

// pickDevScript 挑开发态脚本：优先 dev，其次 serve / start。
//
// 与 pickBuildScript 相反，这里要排除 build——dev 脚本跑了才会起服务，
// 挑成 build 会「启动」出一个立刻退出的进程。
func pickDevScript(scripts map[string]string) string {
	best, bestScore := "", 0
	for name := range scripts {
		l := strings.ToLower(name)
		score := 0
		switch {
		case strings.Contains(l, "dev"):
			score = 4
		case strings.Contains(l, "serve"):
			score = 3
		case strings.Contains(l, "start"):
			score = 2
		}
		if score == 0 || strings.Contains(l, "build") || strings.Contains(l, "test") {
			continue
		}
		if score > bestScore || (score == bestScore && name < best) {
			best, bestScore = name, score
		}
	}
	return best
}

// readFrontendPort 从 vite / vue-cli / webpack 配置里取 dev server 端口。
func readFrontendPort(modDir string) (int, string) {
	viteMatches, _ := filepath.Glob(filepath.Join(modDir, "vite.config.*"))
	for _, f := range viteMatches {
		if v := portAfterKey(f, "server"); v > 0 {
			return v, filepath.Base(f)
		}
	}
	vueMatches, _ := filepath.Glob(filepath.Join(modDir, "vue.config.*"))
	for _, f := range vueMatches {
		if v := portAfterKey(f, "devServer"); v > 0 {
			return v, filepath.Base(f)
		}
	}
	// .env 里的 PORT=xxxx（vite 的 loadEnv 常见写法）
	envMatches, _ := filepath.Glob(filepath.Join(modDir, ".env*"))
	for _, f := range envMatches {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		re := regexp.MustCompile(`(?m)^\s*(?:VITE_)?PORT\s*=\s*(\d+)`)
		if m := re.FindSubmatch(data); len(m) == 2 {
			if n, err := strconv.Atoi(string(m[1])); err == nil {
				return n, filepath.Base(f)
			}
		}
	}
	return 0, ""
}

// portAfterKey 在文件中找到 key（如 server / devServer）之后最近的 port 数字。
//
// 只往后看一段窗口，避免把文件里别处的端口（如代理后端端口）误当成 dev port。
func portAfterKey(path, key string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	s := string(data)
	idx := strings.Index(s, key)
	if idx < 0 {
		return 0
	}
	window := s[idx:]
	if len(window) > 600 {
		window = window[:600]
	}
	re := regexp.MustCompile(`(?m)\bport\s*:\s*(\d+)`)
	if m := re.FindStringSubmatch(window); len(m) == 2 {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// ───────── 依赖加载 ─────────

// DepsCommand 返回该模块「加载依赖」要跑的命令。
//
// 后端用 dependency:go-offline：它把编译、运行、测试所需的依赖以及插件
// 一次性拉齐，正是「离线也能构建」的前提；只 resolve 编译依赖在跑
// spring-boot:run 时仍可能缺件。
// 前端按锁文件判断包管理器——混用 npm 与 pnpm 会重写锁文件，是实打实的破坏。
func DepsCommand(modDir, modType string) string {
	if modType == "frontend" {
		return frontendInstallCommand(modDir)
	}
	return "mvn -U dependency:go-offline"
}

func frontendInstallCommand(modDir string) string {
	switch {
	case fileExistsIn(modDir, "pnpm-lock.yaml"):
		return "pnpm install"
	case fileExistsIn(modDir, "yarn.lock"):
		return "yarn install"
	default:
		return "npm install"
	}
}

func fileExistsIn(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// ───────── 配置填充 ─────────

// FillDevDefaults 为配置里缺启动命令/端口的模块做一次探测并写回。
//
// 只在用户显式点「识别启动方式」时调用——刻意不在启动时静默执行：
// 用户如果把某个模块的命令清空（比如他不用 mvn 而用 IDEA 跑），
// 启动时又自动填回来，就是程序在跟用户较劲。
//
// 返回实际发生变化的条数。
func FillDevDefaults(cfg *Config, base string) int {
	if cfg == nil || strings.TrimSpace(base) == "" || !dirExists(base) {
		return 0
	}
	n := 0
	for pi := range cfg.Projects {
		proj := cfg.Projects[pi]
		for mi := range proj.Modules {
			mod := &cfg.Projects[pi].Modules[mi]
			if strings.TrimSpace(mod.Run) != "" && mod.Port > 0 {
				continue
			}
			dir := ResolveModuleDir(base, proj, *mod)
			hint := DetectDev(dir, mod.Type)
			if strings.TrimSpace(mod.Run) == "" && hint.Found {
				mod.Run = hint.Run
				n++
			}
			if mod.Port == 0 && hint.Port > 0 {
				mod.Port = hint.Port
				n++
			}
		}
	}
	return n
}

// DevHintFor 是给界面用的单模块探测入口。
func DevHintFor(cfg Config, projectName, moduleName string) (DevHint, error) {
	base := strings.TrimSpace(cfg.BasePath)
	p, m, err := FindModule(cfg, projectName, moduleName)
	if err != nil {
		return DevHint{}, err
	}
	return DetectDev(ResolveModuleDir(base, p, m), m.Type), nil
}

// FindModule 按项目名 + 模块名定位模块。
func FindModule(cfg Config, projectName, moduleName string) (ProjectConfig, ModuleConfig, error) {
	for _, p := range cfg.Projects {
		if p.Name != projectName {
			continue
		}
		for _, m := range p.Modules {
			if m.Name == moduleName {
				return p, m, nil
			}
		}
		return ProjectConfig{}, ModuleConfig{}, fmt.Errorf("项目 %s 下没有模块 %s", projectName, moduleName)
	}
	return ProjectConfig{}, ModuleConfig{}, fmt.Errorf("配置里没有项目 %s", projectName)
}
