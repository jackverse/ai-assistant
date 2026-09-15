package winapi

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 端口归属是「杀端口」功能的地基：找不到占用者就等于功能不存在。
// 这里自己开一个监听端口，再问系统「谁占着它」——答案必须是我们自己。
func TestTCPConnectionsFindOwnListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer ln.Close()

	port := uint16(ln.Addr().(*net.TCPAddr).Port)

	conns, err := TCPConnections()
	if err != nil {
		t.Fatalf("取 TCP 表失败: %v", err)
	}
	if len(conns) == 0 {
		t.Fatal("TCP 表为空，明显不对")
	}

	var found *TCPConn
	for i := range conns {
		c := conns[i]
		if c.LocalPort == port && c.Listening() {
			found = &conns[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("没找到端口 %d 的监听记录（共 %d 条连接）", port, len(conns))
	}
	if found.PID != uint32(os.Getpid()) {
		t.Errorf("端口 %d 的占用 PID = %d，想要 %d", port, found.PID, os.Getpid())
	}
	if found.LocalAddr != "127.0.0.1" {
		t.Errorf("监听地址 = %q，想要 127.0.0.1", found.LocalAddr)
	}
}

func TestProcessImagePathSelf(t *testing.T) {
	p, err := ProcessImagePath(uint32(os.Getpid()))
	if err != nil {
		t.Fatalf("取自身映像路径失败: %v", err)
	}
	if !strings.EqualFold(filepath.Base(p), filepath.Base(os.Args[0])) {
		// 测试二进制在 go-build 缓存里，路径不同是正常的；
		// 但文件名必须一致，否则说明读到的是别的进程。
		if !strings.HasSuffix(strings.ToLower(p), ".test.exe") {
			t.Errorf("自身映像路径可疑: %s (argv0=%s)", p, os.Args[0])
		}
	}
}

func TestProcessAliveSelfAndDeadPID(t *testing.T) {
	if !ProcessAlive(uint32(os.Getpid())) {
		t.Error("自己应该是活着的")
	}
	// 4 = System，永远不该被当成「可以杀」的目标
	if err := TerminateProcessByPID(4, 1); err == nil {
		t.Error("终止 PID 4 应该被拒绝")
	}
}

func TestNetPortByteSwap(t *testing.T) {
	// 8080 = 0x1F90，表里按网络序存放在低 16 位 → 读成小端是 0x901F
	if got := netPort(0x0000901F); got != 8080 {
		t.Errorf("netPort(0x901F) = %d，想要 8080", got)
	}
	if got := ipv4String(0x0100007F); got != "127.0.0.1" {
		t.Errorf("ipv4String(0x0100007F) = %s，想要 127.0.0.1", got)
	}
}
