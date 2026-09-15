package winapi

import (
	"fmt"
	"net"
	"syscall"
	"unsafe"
)

// 端口归属查询（GetExtendedTcpTable）。
//
// 为什么用 API 而不是解析 `netstat -ano`：
//   - netstat 要起一个进程、等它输出，一次几百毫秒；界面刷新端口状态时这是可感的延迟。
//   - netstat 的输出列依赖系统语言与列宽，解析规则很脆。
//   - API 调用在毫秒级，且能直接拿到 owning PID，正是「谁占了这个端口」要的信息。

var (
	iphlpapi                = syscall.NewLazyDLL("iphlpapi.dll")
	procGetExtendedTcpTable = iphlpapi.NewProc("GetExtendedTcpTable")
)

const (
	AF_INET  = 2
	AF_INET6 = 23

	// TCP_TABLE_OWNER_PID_ALL：所有 TCP 连接（含 LISTEN），并带 owning PID
	TCP_TABLE_OWNER_PID_ALL = 5
)

// TCP 连接状态（MIB_TCP_STATE）
const (
	TCP_STATE_CLOSED      = 1
	TCP_STATE_LISTEN      = 2
	TCP_STATE_SYN_SENT    = 3
	TCP_STATE_SYN_RCVD    = 4
	TCP_STATE_ESTABLISHED = 5
	TCP_STATE_FIN_WAIT1   = 6
	TCP_STATE_FIN_WAIT2   = 7
	TCP_STATE_CLOSE_WAIT  = 8
	TCP_STATE_CLOSING     = 9
	TCP_STATE_LAST_ACK    = 10
	TCP_STATE_TIME_WAIT   = 11
	TCP_STATE_DELETE_TCB  = 12
)

// TCPConn 是一条 TCP 连接记录（含监听）。
type TCPConn struct {
	Family     uint16 `json:"family"` // 4 / 6
	LocalAddr  string `json:"local_addr"`
	LocalPort  uint16 `json:"local_port"`
	RemoteAddr string `json:"remote_addr"`
	RemotePort uint16 `json:"remote_port"`
	State      uint32 `json:"state"`
	PID        uint32 `json:"pid"`
}

// Listening 判断是否为监听态。
func (c TCPConn) Listening() bool { return c.State == TCP_STATE_LISTEN }

// StateName 返回状态的可读名（与 netstat 输出一致，便于用户对照）。
func TCPStateName(state uint32) string {
	switch state {
	case TCP_STATE_CLOSED:
		return "CLOSED"
	case TCP_STATE_LISTEN:
		return "LISTENING"
	case TCP_STATE_SYN_SENT:
		return "SYN_SENT"
	case TCP_STATE_SYN_RCVD:
		return "SYN_RCVD"
	case TCP_STATE_ESTABLISHED:
		return "ESTABLISHED"
	case TCP_STATE_FIN_WAIT1:
		return "FIN_WAIT1"
	case TCP_STATE_FIN_WAIT2:
		return "FIN_WAIT2"
	case TCP_STATE_CLOSE_WAIT:
		return "CLOSE_WAIT"
	case TCP_STATE_CLOSING:
		return "CLOSING"
	case TCP_STATE_LAST_ACK:
		return "LAST_ACK"
	case TCP_STATE_TIME_WAIT:
		return "TIME_WAIT"
	case TCP_STATE_DELETE_TCB:
		return "DELETE_TCB"
	}
	return fmt.Sprintf("STATE_%d", state)
}

// TCPConnections 返回本机全部 TCP 连接（IPv4 + IPv6，含监听）。
func TCPConnections() ([]TCPConn, error) {
	v4, err := tcpTableRows(AF_INET)
	if err != nil {
		return nil, err
	}
	v6, err6 := tcpTableRows(AF_INET6)
	if err6 != nil && len(v4) == 0 {
		return nil, err6
	}
	return append(v4, v6...), nil
}

// tcpTableRows 取指定地址族的 TCP 表。
//
// 缓冲区分两次调用：先传空缓冲拿到所需大小（返回 ERROR_INSUFFICIENT_BUFFER），
// 再按该大小取数据。这个「两段式」是这类 Win32 表的固定用法。
func tcpTableRows(family uint32) ([]TCPConn, error) {
	var size uint32
	r1, _, _ := procGetExtendedTcpTable.Call(
		0, uintptr(unsafe.Pointer(&size)), 0, uintptr(family), TCP_TABLE_OWNER_PID_ALL, 0)
	const errInsufficientBuffer = 122
	if r1 != errInsufficientBuffer && r1 != 0 {
		return nil, fmt.Errorf("GetExtendedTcpTable 取大小失败: 错误码 %d", r1)
	}
	if size == 0 {
		return nil, nil
	}

	buf := make([]byte, size)
	r1, _, _ = procGetExtendedTcpTable.Call(
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0,
		uintptr(family), TCP_TABLE_OWNER_PID_ALL, 0)
	if r1 != 0 {
		return nil, fmt.Errorf("GetExtendedTcpTable 取数据失败: 错误码 %d", r1)
	}

	if len(buf) < 4 {
		return nil, nil
	}
	n := int(u32(buf, 0))
	out := make([]TCPConn, 0, n)
	offset := 4
	for i := 0; i < n; i++ {
		if family == AF_INET {
			// MIB_TCPROW_OWNER_PID：state, localAddr, localPort, remoteAddr, remotePort, pid（6×4 字节）
			const rowLen = 24
			if offset+rowLen > len(buf) {
				break
			}
			out = append(out, TCPConn{
				Family:     4,
				State:      u32(buf, offset),
				LocalAddr:  ipv4String(u32(buf, offset+4)),
				LocalPort:  netPort(u32(buf, offset+8)),
				RemoteAddr: ipv4String(u32(buf, offset+12)),
				RemotePort: netPort(u32(buf, offset+16)),
				PID:        u32(buf, offset+20),
			})
			offset += rowLen
			continue
		}
		// MIB_TCP6ROW_OWNER_PID：localAddr[16], localScopeId, localPort,
		// remoteAddr[16], remoteScopeId, remotePort, state, pid（共 56 字节）
		const rowLen = 56
		if offset+rowLen > len(buf) {
			break
		}
		out = append(out, TCPConn{
			Family:     6,
			LocalAddr:  ipv6String(buf[offset : offset+16]),
			LocalPort:  netPort(u32(buf, offset+20)),
			RemoteAddr: ipv6String(buf[offset+24 : offset+40]),
			RemotePort: netPort(u32(buf, offset+44)),
			State:      u32(buf, offset+48),
			PID:        u32(buf, offset+52),
		})
		offset += rowLen
	}
	return out, nil
}

// u32 读取小端 32 位整数。
func u32(b []byte, off int) uint32 {
	return uint32(b[off]) | uint32(b[off+1])<<8 | uint32(b[off+2])<<16 | uint32(b[off+3])<<24
}

// netPort 把网络字节序的端口字段还原为主机序端口号。
//
// 表里的端口按网络字节序存在 DWORD 的低 16 位：内存中 8080（0x1F90）
// 存为 0x90 0x1F，读成小端 DWORD 就是 0x901F，需要翻回来。
func netPort(v uint32) uint16 {
	return uint16(v>>8) | uint16(v&0xFF)<<8
}

func ipv4String(v uint32) string {
	// 同理：内存里就是网络序的四个字节，小端读出后逐字节还原
	return net.IPv4(byte(v), byte(v>>8), byte(v>>16), byte(v>>24)).String()
}

func ipv6String(b []byte) string {
	ip := make(net.IP, 16)
	copy(ip, b)
	return ip.String()
}
