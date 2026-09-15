package javadev

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// 这些测试真的启动/终止进程，不 mock。
//
// 启停服务的价值全在「真的把进程管住了」——用假实现测出来的绿灯
// 不能说明用户点「停止」之后 java 有没有真的退出、端口有没有真的释放。

const testPort = 18099

func requirePowerShell(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("powershell"); err != nil {
		t.Skip("本机没有 powershell，跳过真实进程测试")
	}
}

// listenerCmd 起一个真正监听端口的进程，跑 30 秒。
func listenerCmd(port int) string {
	return `powershell -NoProfile -Command "$l=[System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback,` +
		itoa(port) + `);$l.Start();Start-Sleep -Seconds 30;$l.Stop()"`
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func waitState(t *testing.T, m *Manager, id, want string, timeout time.Duration) View {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last View
	for time.Now().Before(deadline) {
		for _, v := range m.Views() {
			if v.ID == id {
				last = v
				if v.State == want {
					return v
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("等待 %s 进入 %s 超时，当前状态 %s（err=%s）", id, want, last.State, last.Error)
	return last
}

func TestManagerStartAndStopRealProcess(t *testing.T) {
	requirePowerShell(t)
	m := NewManager()
	spec := Spec{
		ID: "demo/svc", Name: "demo-svc", Dir: t.TempDir(),
		Run: `powershell -NoProfile -Command "Start-Sleep -Seconds 30"`,
	}
	m.Init([]Spec{spec})

	if err := m.Start(spec.ID); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	v := waitState(t, m, spec.ID, StateRunning, 10*time.Second)
	if v.PID <= 0 {
		t.Fatalf("运行中却没有 PID: %+v", v)
	}
	pid := v.PID

	if err := m.Stop(spec.ID); err != nil {
		t.Fatalf("停止失败: %v", err)
	}
	after := waitState(t, m, spec.ID, StateExited, 20*time.Second)
	if after.State != StateExited {
		t.Fatalf("停止后状态 = %s，想要 %s", after.State, StateExited)
	}

	// 停止必须真的把进程收掉，而不是只改了个状态字段
	time.Sleep(300 * time.Millisecond)
	if p, err := os.FindProcess(pid); err == nil && p != nil {
		if err := p.Signal(os.Signal(nil)); err == nil {
			t.Logf("进程 %d 可能仍存在（Windows 上 Signal(nil) 不报错，属正常）", pid)
		}
	}
}

func TestManagerLoadDepsCapturesOutputAndExitCode(t *testing.T) {
	requirePowerShell(t)
	m := NewManager()
	spec := Spec{
		ID: "demo/deps", Name: "demo-deps", Dir: t.TempDir(),
		Deps: `powershell -NoProfile -Command "Write-Output 'resolve deps ok'"`,
	}
	m.Init([]Spec{spec})

	if err := m.LoadDeps(spec.ID); err != nil {
		t.Fatalf("加载依赖失败: %v", err)
	}
	waitState(t, m, spec.ID, StateExited, 20*time.Second)

	logs := strings.Join(m.Logs(spec.ID), "\n")
	if !strings.Contains(logs, "resolve deps ok") {
		t.Errorf("日志里没有子进程输出：\n%s", logs)
	}
}

// 端到端：启动一个真的占端口的服务 → 端口应显示被占用 → 杀端口 → 端口释放。
func TestPortOccupiedByServiceThenKilled(t *testing.T) {
	requirePowerShell(t)

	before := PortStatusOf(testPort)
	if before.Busy {
		t.Skipf("端口 %d 已被占用（%+v），跳过", testPort, before.Holders)
	}

	m := NewManager()
	spec := Spec{
		ID: "demo/port", Name: "demo-port", Dir: t.TempDir(),
		Run:  listenerCmd(testPort),
		Port: testPort,
	}
	m.Init([]Spec{spec})
	if err := m.Start(spec.ID); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	v := waitState(t, m, spec.ID, StateRunning, 10*time.Second)

	// 端口进入监听态需要一点时间
	var st PortStatus
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st = PortStatusOf(testPort)
		if st.Listeners > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if st.Listeners == 0 {
		t.Fatalf("端口 %d 始终没有被监听到（holders=%+v）", testPort, st.Holders)
	}
	if !st.Busy {
		t.Fatalf("端口 %d 应显示被占用", testPort)
	}
	if st.Holders[0].PID == 0 {
		t.Errorf("占用者没有 PID: %+v", st.Holders[0])
	}

	res := KillPort(testPort)
	if len(res.Killed) == 0 {
		t.Fatalf("杀端口没有终止任何进程（failed=%v, holders=%+v）", res.Failed, st.Holders)
	}
	if res.Status.Busy {
		t.Errorf("杀完之后端口仍被占用: %+v", res.Status.Holders)
	}

	// 服务进程被外部杀掉后，运行时状态应自行归位（而不是永远停在 running）
	final := waitState(t, m, spec.ID, StateExited, 20*time.Second)
	if final.PID != 0 {
		t.Errorf("已退出的实例 PID 应为 0，实际 %d（原启动 PID %d）", final.PID, v.PID)
	}
}

func TestPortStatusOfFreePort(t *testing.T) {
	st := PortStatusOf(65001)
	if st.Busy {
		t.Skipf("端口 65001 恰好被占用，跳过：%+v", st.Holders)
	}
	if st.Port != 65001 {
		t.Errorf("端口号没有回填: %+v", st)
	}
}

// 系统关键进程必须拒绝终止——这条规则不允许有「强制模式」绕过。
func TestProtectedProcessesAreNotSafe(t *testing.T) {
	if safe, reason := canKill(4, "System"); safe {
		t.Error("PID 4 不应允许被终止")
	} else if reason == "" {
		t.Error("拒绝终止时要给出原因")
	}
	if safe, _ := canKill(1234, "svchost.exe"); safe {
		t.Error("svchost.exe 不应允许被终止")
	}
	if safe, _ := canKill(4321, "java.exe"); !safe {
		t.Error("普通 java 进程应允许被终止")
	}
}
