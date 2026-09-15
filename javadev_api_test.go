package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"winclean/internal/config"
	"winclean/internal/deploy"
)

// 这一组测试跑的是「配置 → 服务清单 → 绑定 DTO → JSON」整条链路。
//
// 为什么值得测：前端拿到的是 JSON，字段名对不上时不会报错，只会显示空白——
// 这类问题在界面上极难定位。这里把契约钉住。
//
// 注意：测试把 APPDATA 改到临时目录，避免碰到用户真实的 config.yaml。

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newTestApp 造一个带假项目的 App。
func newTestApp(t *testing.T) (*App, string) {
	t.Helper()
	t.Setenv("APPDATA", t.TempDir())

	base := t.TempDir()
	// 后端模块：有 spring-boot 插件、application-dev.yml 里写了端口
	writeTestFile(t, filepath.Join(base, "demo", "svc", "pom.xml"),
		`<project><artifactId>svc</artifactId><build><plugins>
		 <plugin><artifactId>spring-boot-maven-plugin</artifactId></plugin>
		 </plugins></build></project>`)
	writeTestFile(t, filepath.Join(base, "demo", "svc", "src", "main", "resources", "application.yml"),
		"spring:\n  profiles:\n    active: dev\n")
	writeTestFile(t, filepath.Join(base, "demo", "svc", "src", "main", "resources", "application-dev.yml"),
		"server:\n  port: 18077\n")

	cfg := config.Default()
	cfg.Deploy = deploy.Config{
		BasePath: base,
		Projects: []deploy.ProjectConfig{{
			Name: "demo", Root: "demo",
			Modules: []deploy.ModuleConfig{
				{Name: "svc", Type: "backend", Root: "svc", Run: "mvn spring-boot:run", Port: 18077},
			},
		}},
	}
	return &App{cfg: cfg}, base
}

func TestGetDevPageContract(t *testing.T) {
	a, base := newTestApp(t)

	dto := a.GetDevPage(false)
	if len(dto.Services) != 1 {
		t.Fatalf("应有 1 个服务，实际 %d（notes=%v）", len(dto.Services), dto.Notes)
	}
	s := dto.Services[0]
	if s.ID != "demo/svc" {
		t.Errorf("服务 id = %q，想要 demo/svc", s.ID)
	}
	if !s.Ready {
		t.Errorf("模块应可启动，实际 ready=false reason=%q", s.Reason)
	}
	if s.Port != 18077 {
		t.Errorf("端口 = %d，想要 18077", s.Port)
	}
	if s.State != "stopped" {
		t.Errorf("初始状态 = %q，想要 stopped", s.State)
	}
	if s.Dir != filepath.Join(base, "demo", "svc") {
		t.Errorf("模块目录 = %q", s.Dir)
	}
	// 依赖命令由类型推导，前端要显示它，所以必须随 DTO 下发
	if !strings.Contains(s.Deps, "dependency:go-offline") {
		t.Errorf("依赖命令 = %q，想要 mvn dependency:go-offline", s.Deps)
	}
	if s.PortStatus == nil {
		t.Error("带端口的服务应附端口占用状态（前端据此显示空闲/被占用）")
	}

	// 字段名是前端的契约，逐个钉住
	b, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"base_path", "summary", "services", "notes", "jdk", "maven"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("DevPageDTO 缺少字段 %s", k)
		}
	}
	svc := raw["services"].([]any)[0].(map[string]any)
	for _, k := range []string{"id", "name", "project", "module", "type", "dir",
		"run", "deps", "port", "ready", "state", "pid", "logs", "port_status"} {
		if _, ok := svc[k]; !ok {
			t.Errorf("DevServiceDTO 缺少字段 %s", k)
		}
	}
	// 工具链路径不下发给界面（JSON tag 为 -），避免把本机路径暴露成契约
	if _, ok := svc["jdk"]; ok {
		t.Error("JDK 路径不应作为服务字段下发")
	}
}

func TestGetDevPageWithoutConfig(t *testing.T) {
	t.Setenv("APPDATA", t.TempDir())
	a := &App{cfg: config.Default()}

	dto := a.GetDevPage(false)
	if len(dto.Services) != 0 {
		t.Errorf("没配置时不该有服务，实际 %d 个", len(dto.Services))
	}
	if len(dto.Notes) == 0 {
		t.Error("没配置时要给出「去哪里配置」的提示，而不是一块空白")
	}
}

func TestSaveDevServicePersistsAndReloads(t *testing.T) {
	a, _ := newTestApp(t)

	if err := a.SaveDevService("demo", "svc", `mvn spring-boot:run -Dspring-boot.run.profiles=test`, 18078); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	// 内存里的配置与再读一次磁盘的配置都要是新值
	nc, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	_, mod, err := deploy.FindModule(nc.Deploy, "demo", "svc")
	if err != nil {
		t.Fatal(err)
	}
	if mod.Port != 18078 {
		t.Errorf("落盘端口 = %d，想要 18078", mod.Port)
	}
	if !strings.Contains(mod.Run, "profiles=test") {
		t.Errorf("落盘命令 = %q", mod.Run)
	}

	dto := a.GetDevPage(false)
	if dto.Services[0].Port != 18078 {
		t.Errorf("保存后服务清单端口 = %d，想要 18078", dto.Services[0].Port)
	}

	if err := a.SaveDevService("demo", "nope", "x", 1); err == nil {
		t.Error("保存不存在的模块应该报错")
	}
	if err := a.SaveDevService("demo", "svc", "mvn x", 70000); err == nil {
		t.Error("端口越界应该报错")
	}
}

func TestDetectAllDevServicesFillsFromDisk(t *testing.T) {
	a, _ := newTestApp(t)
	// 清掉已填好的，模拟用户从旧配置升级上来的状态
	a.cfg.Deploy.Projects[0].Modules[0].Run = ""
	a.cfg.Deploy.Projects[0].Modules[0].Port = 0

	res, err := a.DetectAllDevServices()
	if err != nil {
		t.Fatalf("识别失败: %v", err)
	}
	if res.Filled == 0 {
		t.Fatalf("应从 pom.xml 与 application-dev.yml 补出命令与端口，实际 0 项")
	}
	mod := a.cfg.Deploy.Projects[0].Modules[0]
	if mod.Run != "mvn spring-boot:run" {
		t.Errorf("识别到的命令 = %q", mod.Run)
	}
	if mod.Port != 18077 {
		t.Errorf("识别到的端口 = %d，想要 18077", mod.Port)
	}
}

func TestStartDevServiceRejectsUnreadyAndBusyPort(t *testing.T) {
	a, _ := newTestApp(t)

	// 未设置启动命令的模块：直接说清原因，而不是启动一个必然失败的进程
	a.cfg.Deploy.Projects[0].Modules[0].Run = ""
	if err := a.StartDevService("demo/svc"); err == nil {
		t.Error("没有启动命令时应拒绝启动")
	}

	// 端口被占用时应在启动前拦下，并指出占用者
	a.cfg.Deploy.Projects[0].Modules[0].Run = `powershell -NoProfile -Command "Start-Sleep -Seconds 5"`
	ln, port := listenOnFreePort(t)
	defer ln.Close()
	a.cfg.Deploy.Projects[0].Modules[0].Port = port

	err := a.StartDevService("demo/svc")
	if err == nil {
		t.Fatal("端口被占用时应拒绝启动")
	}
	if !strings.Contains(err.Error(), "已被占用") {
		t.Errorf("错误信息应说明端口被占用，实际: %v", err)
	}
}

func TestCheckPortDTOShape(t *testing.T) {
	a, _ := newTestApp(t)
	st, err := a.CheckPort(18077)
	if err != nil {
		t.Fatalf("查端口失败: %v", err)
	}
	if st.Port != 18077 {
		t.Errorf("端口号 = %d", st.Port)
	}
	if _, err := a.CheckPort(0); err == nil {
		t.Error("端口 0 应该报错")
	}
	if _, err := a.CheckPort(70000); err == nil {
		t.Error("越界端口应该报错")
	}
}

func TestGitChangesOnNonRepo(t *testing.T) {
	a, _ := newTestApp(t) // 临时目录不是 git 仓库
	repos := a.GitChanges()
	if len(repos) != 1 {
		t.Fatalf("应有 1 个项目，实际 %d", len(repos))
	}
	if repos[0].IsRepo {
		t.Error("临时目录不该被判定为 git 仓库")
	}
	if repos[0].Err == "" {
		t.Error("不在仓库里时要给出原因")
	}
}

// listenOnFreePort 占一个空闲端口（用于验证「端口被占用时拒绝启动」）。
func listenOnFreePort(t *testing.T) (net.Listener, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	return ln, ln.Addr().(*net.TCPAddr).Port
}
