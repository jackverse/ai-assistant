// Package deploy 实现 Java 项目的打包部署能力（助手的第三个功能入口）。
//
// 设计文档见 docs/design/15-Java打包部署模块设计.md。
//
// 与既有工具（bundle-packer）的关系：能力对标，但改进两点——
//  1. Maven/JDK 路径自动探测，不用手填绝对路径；
//  2. 输出目录可在界面上选择。
//
// 本包只用标准库，不引入外部依赖。
package deploy

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Toolchain 是一个被探测到的工具链安装。
type Toolchain struct {
	Kind    string `json:"kind"`    // "jdk" | "maven"
	Path    string `json:"path"`    // JDK 为根目录；Maven 为 mvn.cmd 的完整路径
	Root    string `json:"root"`    // 所属安装根目录（界面展示用）
	Version string `json:"version"` // 尽量探测到；探不到为空
	Source  string `json:"source"`  // 来源说明（哪个规则找到的），便于排查「为什么没找到我的 JDK」
	Valid   bool   `json:"valid"`   // 关键可执行文件是否存在
	Detail  string `json:"detail"`  // 校验失败的说明
}

// DetectResult 是一次完整的工具链探测结果。
type DetectResult struct {
	JDKs    []Toolchain `json:"jdks"`
	Mavens  []Toolchain `json:"mavens"`
	Notes   []string    `json:"notes"`   // 探测过程中的提示（例如 JAVA_HOME 指向不存在的目录）
	EnvJAVA string      `json:"env_java_home"`
	EnvMvn  string      `json:"env_maven_home"`
}

// Detect 探测本机的 JDK 与 Maven。
//
// 探测策略（先廉价后昂贵）：
//  1. 环境变量 JAVA_HOME / MAVEN_HOME / M2_HOME —— 最权威，优先
//  2. PATH 中的 java.exe / mvn.cmd —— 用户日常命令行能用的那个
//  3. 常见安装根下的一级目录 —— 覆盖换机器、装了多个版本的情况
//  4. 代码目录下的 mvnw / mvnw.cmd（Maven Wrapper）—— 项目自带的版本，优先级判断留给用户
//
// 刻意不启动任何进程（不用 `mvn -v` / `java -version`）：那会拖慢探测，
// 且在没有 JDK 的机器上还会弹错误框。版本信息统一从 JDK 自带的 release 文件读取。
func Detect(extraSearchRoots []string) DetectResult {
	var res DetectResult

	res.EnvJAVA = os.Getenv("JAVA_HOME")
	res.EnvMvn = firstNonEmpty(os.Getenv("MAVEN_HOME"), os.Getenv("M2_HOME"))

	seenJDK := map[string]bool{}
	seenMvn := map[string]bool{}

	// ── 1. 环境变量 ────────────────────────────────────────
	if res.EnvJAVA != "" {
		if jdk, ok := inspectJDK(res.EnvJAVA, "环境变量 JAVA_HOME"); ok {
			addJDK(&res, jdk, seenJDK)
		} else {
			res.Notes = append(res.Notes,
				"JAVA_HOME 指向的目录不是有效 JDK（缺 bin\\javac.exe）："+res.EnvJAVA)
		}
	}
	if res.EnvMvn != "" {
		if mvn, ok := inspectMavenBin(filepath.Join(res.EnvMvn, "bin", "mvn.cmd"),
			"环境变量 MAVEN_HOME"); ok {
			addMaven(&res, mvn, seenMvn)
		} else {
			res.Notes = append(res.Notes,
				"MAVEN_HOME 下没找到 bin\\mvn.cmd："+res.EnvMvn)
		}
	}

	// ── 2. PATH ───────────────────────────────────────────
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		if jdk, ok := inspectJDK(dir, "PATH"); ok {
			addJDK(&res, jdk, seenJDK)
			continue
		}
		if mvn, ok := inspectMavenBin(filepath.Join(dir, "mvn.cmd"), "PATH"); ok {
			addMaven(&res, mvn, seenMvn)
		}
	}

	// ── 3. 常见安装根 ─────────────────────────────────────
	for _, jdkPattern := range jdkSearchPatterns() {
		matches, _ := filepath.Glob(jdkPattern)
		// 版本号高的排前面，默认选中会落在最新版本上
		sort.Sort(sort.Reverse(sort.StringSlice(matches)))
		for _, m := range matches {
			if jdk, ok := inspectJDK(m, "安装目录扫描"); ok {
				addJDK(&res, jdk, seenJDK)
			}
		}
	}
	for _, mvnPattern := range mavenSearchPatterns() {
		matches, _ := filepath.Glob(mvnPattern)
		sort.Sort(sort.Reverse(sort.StringSlice(matches)))
		for _, m := range matches {
			if mvn, ok := inspectMavenBin(filepath.Join(m, "bin", "mvn.cmd"), "安装目录扫描"); ok {
				addMaven(&res, mvn, seenMvn)
			}
		}
	}

	// ── 4. 代码根下的 Maven Wrapper ────────────────────────
	//
	// 性能注意：这里只扫【一层】。实测（本机 D:\code）用两层通配
	// `*/*/mvnw.cmd` 会把几百个项目下的所有子目录都枚举一遍，耗时 13 秒；
	// 收敛为一层后降到 1 秒内。项目根通常直接放 mvnw，一层足够。
	// 结果数量也设上限，避免代码根特别大时拖慢整个探测。
	const maxWrappers = 50
	for _, root := range extraSearchRoots {
		if root == "" || len(wrapperCount(&res)) >= maxWrappers {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(root, "*", "mvnw.cmd"))
		for _, m := range matches {
			if len(wrapperCount(&res)) >= maxWrappers {
				break
			}
			tc := Toolchain{
				Kind: "maven", Path: m, Root: filepath.Dir(m),
				Source: "项目自带 Maven Wrapper", Valid: true,
				Version: "wrapper",
			}
			addMaven(&res, tc, seenMvn)
		}
	}

	// 排序：有效的在前，同有效性按版本倒序（版本高的更可能是用户想用的）
	sortToolchains(res.JDKs)
	sortToolchains(res.Mavens)
	return res
}

func addJDK(res *DetectResult, tc Toolchain, seen map[string]bool) {
	key := strings.ToLower(filepath.Clean(tc.Path))
	if seen[key] {
		return
	}
	seen[key] = true
	res.JDKs = append(res.JDKs, tc)
}

func addMaven(res *DetectResult, tc Toolchain, seen map[string]bool) {
	key := strings.ToLower(filepath.Clean(tc.Path))
	if seen[key] {
		return
	}
	seen[key] = true
	res.Mavens = append(res.Mavens, tc)
}

// wrapperCount 统计当前已收集的 Maven Wrapper 数量（用于限制扫描规模）。
func wrapperCount(res *DetectResult) []Toolchain {
	var out []Toolchain
	for _, m := range res.Mavens {
		if m.Version == "wrapper" {
			out = append(out, m)
		}
	}
	return out
}

// sortToolchains 排序：有效的在前；同为有效时，
// 有真实版本号的（环境变量/PATH/安装目录找到的）优先于项目自带的 wrapper——
// 默认选中会落在用户日常使用的那个 mvn 上，而不是某个项目里的 wrapper。
func sortToolchains(list []Toolchain) {
	rank := func(t Toolchain) int {
		if !t.Valid {
			return 0
		}
		if t.Version == "wrapper" {
			return 1
		}
		return 2
	}
	sort.SliceStable(list, func(i, j int) bool {
		ri, rj := rank(list[i]), rank(list[j])
		if ri != rj {
			return ri > rj
		}
		return list[i].Version > list[j].Version
	})
}

// inspectJDK 判断目录是否是 JDK 根（含 bin\javac.exe）。
//
// 只用 javac 而不是 java 作为判据：装了 java 但没有 javac 的是 JRE，
// 用 JRE 构建会失败，因此不能算作可用工具链。
func inspectJDK(dir, source string) (Toolchain, bool) {
	dir = filepath.Clean(dir)
	javac := filepath.Join(dir, "bin", "javac.exe")
	if !isFile(javac) {
		return Toolchain{}, false
	}
	tc := Toolchain{
		Kind: "jdk", Path: dir, Root: dir, Source: source, Valid: true,
	}
	tc.Version = readJDKVersion(dir)
	return tc, true
}

// inspectMavenBin 判断给定路径是否是可用的 mvn.cmd。
func inspectMavenBin(mvnCmd, source string) (Toolchain, bool) {
	mvnCmd = filepath.Clean(mvnCmd)
	if !isFile(mvnCmd) {
		return Toolchain{}, false
	}
	root := filepath.Dir(filepath.Dir(mvnCmd))
	return Toolchain{
		Kind: "maven", Path: mvnCmd, Root: root,
		Source: source, Valid: true,
		Version: readMavenVersion(root),
	}, true
}

// readMavenVersion 从 Maven 安装目录推断版本号。
//
// 不启动 `mvn -v`（慢且依赖 JDK 可用）。Maven 发行包的 lib 目录里
// 必有 maven-core-<version>.jar，从文件名取版本最快且零副作用。
func readMavenVersion(mavenRoot string) string {
	matches, _ := filepath.Glob(filepath.Join(mavenRoot, "lib", "maven-core-*.jar"))
	for _, m := range matches {
		base := filepath.Base(m)
		v := strings.TrimSuffix(strings.TrimPrefix(base, "maven-core-"), ".jar")
		if v != "" && v != base {
			return v
		}
	}
	// 退路：目录名里常带版本（apache-maven-3.9.6）
	if m := versionFromDirName(filepath.Base(mavenRoot)); m != "" {
		return m
	}
	return ""
}

// versionFromDirName 从形如 apache-maven-3.9.6 / jdk-17.0.9 的目录名里提取版本。
func versionFromDirName(name string) string {
	parts := strings.FieldsFunc(name, func(r rune) bool {
		return r == '-' || r == '_' || r == ' '
	})
	for i := len(parts) - 1; i >= 0; i-- {
		p := parts[i]
		if p == "" {
			continue
		}
		// 版本特征：以数字开头且含点，或纯数字
		if p[0] >= '0' && p[0] <= '9' && strings.ContainsAny(p, ".0123456789") {
			return p
		}
	}
	return ""
}

// readJDKVersion 从 JDK 根目录的 release 文件读取版本。
//
// release 是 JDK 9+ 都有的元数据文件，内容形如：
//
//	JAVA_VERSION="17.0.9"
//
// 比启动 `java -version` 快得多，也不需要解析 stderr。
func readJDKVersion(jdkRoot string) string {
	f, err := os.Open(filepath.Join(jdkRoot, "release"))
	if err != nil {
		return ""
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "JAVA_VERSION=") {
			continue
		}
		v := strings.TrimPrefix(line, "JAVA_VERSION=")
		return strings.Trim(v, `"`)
	}
	return ""
}

// jdkSearchPatterns 返回 JDK 的常见安装位置。
//
// 覆盖：Oracle/OpenJDK 官方安装、Adoptium（Eclipse Temurin）、Microsoft Build、
// 以及开发者常放的 D:\workplace、D:\dev、.jdks（IntelliJ 下载的 JDK）。
func jdkSearchPatterns() []string {
	var out []string
	drives := []string{`C:\`, `D:\`}

	systemRoots := []string{
		`Program Files\Java\jdk*`,
		`Program Files\Java\jdk-*`,
		`Program Files\Eclipse Adoptium\jdk-*`,
		`Program Files\Microsoft\jdk-*`,
		`Program Files\Amazon Corretto\jdk*`,
		`Program Files\Zulu\zulu-*`,
		`Program Files\BellSoft\LibericaJDK-*`,
	}
	for _, d := range drives {
		for _, s := range systemRoots {
			out = append(out, filepath.Join(d, s))
		}
	}

	// 开发者惯用目录（本机实测：JAVA_HOME=D:\workplace\jdk17）
	//
	// 只用【一层】通配。两层（如 dev\*\jdk*）会枚举根下每个项目的所有子目录，
	// 在代码目录大的机器上会让探测慢到十几秒——实测过这个坑。
	for _, d := range drives {
		out = append(out,
			filepath.Join(d, "workplace", "jdk*"),
			filepath.Join(d, "workplace", "*jdk*"),
			filepath.Join(d, "dev", "jdk*"),
			filepath.Join(d, "software", "jdk*"),
			filepath.Join(d, "soft", "jdk*"),
			filepath.Join(d, "tools", "jdk*"),
			filepath.Join(d, "java", "jdk*"),
		)
	}

	if home, err := os.UserHomeDir(); err == nil {
		out = append(out,
			filepath.Join(home, ".jdks", "*"),
			filepath.Join(home, "scoop", "apps", "openjdk*", "*"),
		)
	}
	return out
}

// mavenSearchPatterns 返回 Maven 的常见安装位置（同样只用一层通配）。
func mavenSearchPatterns() []string {
	var out []string
	for _, d := range []string{`C:\`, `D:\`} {
		out = append(out,
			filepath.Join(d, "Program Files", "apache-maven-*"),
			filepath.Join(d, "Program Files", "apache-maven*"),
			filepath.Join(d, "workplace", "apache-maven-*"), // 本机实测位置
			filepath.Join(d, "workplace", "*maven*"),
			filepath.Join(d, "dev", "apache-maven-*"),
			filepath.Join(d, "software", "apache-maven-*"),
			filepath.Join(d, "soft", "*maven*"),
			filepath.Join(d, "tools", "apache-maven-*"),
			filepath.Join(d, "maven*"),
		)
	}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out,
			filepath.Join(home, "scoop", "apps", "maven", "*", "apache-maven-*"),
			filepath.Join(home, "apache-maven-*"),
		)
	}
	return out
}

// LocalRepoDir 返回本机 Maven 本地仓库路径（用于 idea 模式解析依赖）。
func LocalRepoDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".m2", "repository")
		if dirExists(p) {
			return p
		}
	}
	return ""
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// Platform 返回当前平台，供界面提示（本模块目前只面向 Windows）。
func Platform() string { return runtime.GOOS }
