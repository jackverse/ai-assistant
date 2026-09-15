package deploy

import (
	"os"
	"path/filepath"
	"testing"
)

// server.port 必须只在 server 块里找。实测的 application-dev.yml 里
// 还有 redis / management 等多处 port，全局搜 "port:" 会取到错的值，
// 用户点启动时就会去杀错端口。
func TestReadSpringPortOnlyInServerBlock(t *testing.T) {
	dir := t.TempDir()
	writeFileT(t, filepath.Join(dir, "application.yml"), `spring:
  profiles:
    active: dev
`)
	writeFileT(t, filepath.Join(dir, "application-dev.yml"), `spring:
  redis:
    port: 6379
server:
  port: 8081
  tomcat:
    threads:
      max: 1000
management:
  server:
    port: 9090
`)
	port, src := readSpringPort(dir)
	if port != 8081 {
		t.Errorf("端口 = %d（取自 %s），想要 8081", port, src)
	}
	if src != "application-dev.yml" {
		t.Errorf("来源 = %s，想要 application-dev.yml（即 spring.profiles.active 指向的文件）", src)
	}
}

func TestReadSpringPortProperties(t *testing.T) {
	dir := t.TempDir()
	writeFileT(t, filepath.Join(dir, "application.properties"), "server.port=8090\nspring.redis.port=6379\n")
	if port, _ := readSpringPort(dir); port != 8090 {
		t.Errorf("端口 = %d，想要 8090", port)
	}
}

// dev 脚本挑错会让「启动服务」变成一个立刻退出的构建过程。
func TestPickDevScriptPrefersDevOverBuild(t *testing.T) {
	scripts := map[string]string{
		"admin dev":        "vite",
		"admin build":      "npm run admin build:prod",
		"lint":             "eslint .",
		"admin build:test": "vite build --mode development.test",
	}
	if got := pickDevScript(scripts); got != "admin dev" {
		t.Errorf("挑到 %q，想要 admin dev", got)
	}
	if got := pickDevScript(map[string]string{"build": "vite build", "test": "vitest"}); got != "" {
		t.Errorf("没有 dev/serve/start 脚本时应返回空，实际 %q", got)
	}
	if got := pickDevScript(map[string]string{"serve": "vite preview"}); got != "serve" {
		t.Errorf("挑到 %q，想要 serve", got)
	}
}

func TestDetectDevBackend(t *testing.T) {
	dir := t.TempDir()
	writeFileT(t, filepath.Join(dir, "pom.xml"), `<project><artifactId>demo</artifactId>
  <build><plugins><plugin><artifactId>spring-boot-maven-plugin</artifactId></plugin></plugins></build>
</project>`)
	writeFileT(t, filepath.Join(dir, "src", "main", "resources", "application.yml"), `spring:
  profiles:
    active: dev
`)
	writeFileT(t, filepath.Join(dir, "src", "main", "resources", "application-dev.yml"), "server:\n  port: 8089\n")

	h := DetectDev(dir, "backend")
	if !h.Found || h.Run != "mvn spring-boot:run" {
		t.Errorf("启动命令 = %q (found=%v)，想要 mvn spring-boot:run", h.Run, h.Found)
	}
	if h.Port != 8089 {
		t.Errorf("端口 = %d，想要 8089", h.Port)
	}

	// 没有 spring-boot 插件时不猜命令：宁可留空让用户填
	plain := t.TempDir()
	writeFileT(t, filepath.Join(plain, "pom.xml"), `<project><artifactId>plain</artifactId></project>`)
	if h := DetectDev(plain, "backend"); h.Found {
		t.Errorf("普通 war 项目不该猜出启动命令，实际得到 %q", h.Run)
	}
}

func TestDetectDevFrontend(t *testing.T) {
	dir := t.TempDir()
	writeFileT(t, filepath.Join(dir, "package.json"),
		`{"name":"web","scripts":{"admin dev":"vite","admin build:prod":"vite build --mode production"}}`)
	writeFileT(t, filepath.Join(dir, "vite.config.ts"), `export default defineConfig({
  plugins: [vue()],
  server: { open: true, host: true, port: 8001, hmr: { overlay: false } }
});`)

	h := DetectDev(dir, "frontend")
	// 脚本名含空格，命令必须带引号，否则 npm 会把 "dev" 当成传给脚本的参数
	if h.Run != `npm run "admin dev"` {
		t.Errorf("启动命令 = %q，想要 npm run \"admin dev\"", h.Run)
	}
	if h.Port != 8001 {
		t.Errorf("端口 = %d，想要 8001", h.Port)
	}
}

func TestDepsCommandPicksPackageManagerByLockfile(t *testing.T) {
	be := t.TempDir()
	if got := DepsCommand(be, "backend"); got != "mvn -U dependency:go-offline" {
		t.Errorf("后端依赖命令 = %q", got)
	}
	fe := t.TempDir()
	if got := DepsCommand(fe, "frontend"); got != "npm install" {
		t.Errorf("无锁文件时应为 npm install，实际 %q", got)
	}
	writeFileT(t, filepath.Join(fe, "pnpm-lock.yaml"), "lockfileVersion: 5.4\n")
	if got := DepsCommand(fe, "frontend"); got != "pnpm install" {
		t.Errorf("有 pnpm 锁文件时应为 pnpm install，实际 %q", got)
	}
}

// TestDetectDevAgainstRealProject 用本机真实项目核对探测结果。
//
// 探测逻辑的价值全在「对真实工程有效」——本机 wic-sh 的三个后端模块
// 端口各不相同（8080/8081/8089），前端脚本名带空格，都是容易猜错的形态。
// 目录不存在时跳过，测试保持可移植。
func TestDetectDevAgainstRealProject(t *testing.T) {
	root := `D:\code\wic-sh`
	if _, err := os.Stat(root); err != nil {
		t.Skip("本机没有 D:\\code\\wic-sh，跳过真实项目核对")
	}

	cases := []struct {
		dir      string
		typ      string
		wantRun  string
		wantPort int
	}{
		{"wic-admin", "backend", "mvn spring-boot:run", 8081},
		{"wic-pub", "backend", "mvn spring-boot:run", 8080},
		{"wic-task", "backend", "mvn spring-boot:run", 8089},
		{"wic-admin-web", "frontend", `npm run "admin dev"`, 8001},
	}
	for _, c := range cases {
		dir := filepath.Join(root, c.dir)
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		h := DetectDev(dir, c.typ)
		if h.Run != c.wantRun {
			t.Errorf("%s 启动命令 = %q，想要 %q", c.dir, h.Run, c.wantRun)
		}
		if h.Port != c.wantPort {
			t.Errorf("%s 端口 = %d，想要 %d（%s）", c.dir, h.Port, c.wantPort, h.Note)
		}
	}
}
