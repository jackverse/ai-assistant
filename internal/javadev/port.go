package javadev

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"winclean/internal/winapi"
)

// 端口占用检测与「杀端口」。
//
// 这是开发时最实际的痛点：上一次跑的服务没退干净、或另一个项目占了同一个端口，
// 报错信息通常只说「端口已被占用」，不说被谁占。这里直接给出 PID 与进程名，
// 并允许一键终止——但系统关键进程一律拒绝，且明确告诉用户为什么拒绝。

// Holder 是占用某个端口的一个进程。
type Holder struct {
	PID     uint32 `json:"pid"`
	Process string `json:"process"`
	Path    string `json:"path"`
	Addr    string `json:"local_addr"`
	State   string `json:"state"`
	// Safe 为 false 表示本工具不会去终止它（系统关键进程 / 本程序自身）
	Safe   bool   `json:"safe"`
	Reason string `json:"reason,omitempty"`
}

// PortStatus 是一个端口的占用情况。
type PortStatus struct {
	Port    int      `json:"port"`
	Busy    bool     `json:"busy"`
	Holders []Holder `json:"holders"`
	// Listeners 只统计处于 LISTENING 的占用者：连到该端口的客户端连接
	// 不算「端口被占用」，把它们列出来只会让用户困惑。
	Listeners int `json:"listeners"`
}

// 绝不允许终止的进程名（小写比较）。
//
// 这些进程被杀会导致系统蓝屏、登录会话失效或桌面重启；
// 即便用户明确点了「杀端口」，也不该让一个清理工具干出这种事。
var protectedProcesses = map[string]string{
	"system":          "Windows 内核进程",
	"idle":            "Windows 空闲进程",
	"registry":        "注册表进程",
	"memcompression":  "内存压缩进程",
	"smss.exe":        "会话管理器",
	"csrss.exe":       "客户端服务器运行时",
	"wininit.exe":     "Windows 初始化进程",
	"winlogon.exe":    "登录进程",
	"services.exe":    "服务控制管理器",
	"lsass.exe":       "本地安全认证",
	"svchost.exe":     "系统服务宿主（一个进程里跑着多个系统服务）",
	"fontdrvhost.exe": "字体驱动宿主",
	"dwm.exe":         "桌面窗口管理器",
	"audiodg.exe":     "音频设备图",
	"spoolsv.exe":     "打印后台处理",
	"explorer.exe":    "资源管理器（桌面与任务栏）",
	"wininit":         "Windows 初始化进程",
}

// PortStatusOf 查询单个端口的占用情况。
func PortStatusOf(port int) PortStatus {
	return portStatusFrom(port, collectTCP())
}

// PortStatusBatch 批量查询多个端口，共用一次 TCP 表读取。
//
// 服务列表一次要查 6 个以上端口，逐个查会重复读 6 次系统表；
// 一次读表再分派既准（同一时刻的快照）又快。
func PortStatusBatch(ports []int) map[int]PortStatus {
	conns := collectTCP()
	out := make(map[int]PortStatus, len(ports))
	for _, p := range ports {
		if p <= 0 {
			continue
		}
		out[p] = portStatusFrom(p, conns)
	}
	return out
}

func collectTCP() []winapi.TCPConn {
	conns, err := winapi.TCPConnections()
	if err != nil {
		return nil
	}
	return conns
}

func portStatusFrom(port int, conns []winapi.TCPConn) PortStatus {
	st := PortStatus{Port: port}
	if port <= 0 {
		return st
	}

	// 同一个 PID 可能既在监听又有已建立的连接：按 PID 合并，
	// 状态取监听（监听才是「这个端口被谁占了」的答案）。
	byPID := map[uint32]*Holder{}
	order := []uint32{}
	for _, c := range conns {
		if int(c.LocalPort) != port {
			continue
		}
		h, ok := byPID[c.PID]
		if !ok {
			h = &Holder{
				PID:     c.PID,
				Process: winapi.ProcessName(c.PID),
				Addr:    c.LocalAddr,
				State:   winapi.TCPStateName(c.State),
			}
			if h.Process == "" {
				h.Process = "(未知)"
			}
			byPID[c.PID] = h
			order = append(order, c.PID)
		}
		if c.Listening() {
			h.State = winapi.TCPStateName(c.State)
			h.Addr = c.LocalAddr
			st.Listeners++
		}
	}

	for _, pid := range order {
		h := byPID[pid]
		if path, err := winapi.ProcessImagePath(pid); err == nil {
			h.Path = path
		}
		h.Safe, h.Reason = canKill(pid, h.Process)
		st.Holders = append(st.Holders, *h)
	}
	// 监听者排前面：用户要解决的是「谁占着端口」
	sort.SliceStable(st.Holders, func(i, j int) bool {
		return st.Holders[i].State == "LISTENING" && st.Holders[j].State != "LISTENING"
	})
	st.Busy = len(st.Holders) > 0
	return st
}

// canKill 判断这个进程是否允许被本工具终止。
func canKill(pid uint32, name string) (bool, string) {
	if pid == 0 || pid == 4 {
		return false, "Windows 内核进程，绝不可终止"
	}
	if pid == uint32(os.Getpid()) {
		return false, "这是助手自身的进程"
	}
	if reason, hit := protectedProcesses[strings.ToLower(name)]; hit {
		return false, "系统关键进程：" + reason
	}
	return true, ""
}

// KillResult 是一次「杀端口」的结果。
type KillResult struct {
	Port   int        `json:"port"`
	Killed []Holder   `json:"killed"`
	Failed []string   `json:"failed"`
	Status PortStatus `json:"status"` // 终止后再查一次，让界面直接看到结果
}

// KillPort 终止占用该端口的进程。
//
// 只临时杀、不做任何持久化操作：不动服务注册表、不动开机自启、不动防火墙。
// 刻意不提供「强制模式」——系统关键进程就是不给杀，没有一个开关能绕过，
// 否则它迟早会被误用成「一键杀干净」。
func KillPort(port int) KillResult {
	res := KillResult{Port: port}
	st := PortStatusOf(port)
	if !st.Busy {
		res.Status = st
		return res
	}

	for _, h := range st.Holders {
		if !h.Safe {
			res.Failed = append(res.Failed,
				fmt.Sprintf("PID %d (%s) 已跳过：%s", h.PID, h.Process, h.Reason))
			continue
		}
		if err := winapi.TerminateProcessByPID(h.PID, 1); err != nil {
			res.Failed = append(res.Failed, fmt.Sprintf("PID %d (%s) 终止失败：%v", h.PID, h.Process, err))
			continue
		}
		res.Killed = append(res.Killed, h)
	}

	// 终止是异步的：端口释放需要一点时间，等一下再复查，
	// 否则界面会看到「已杀掉」但仍然「被占用」，用户以为没生效。
	if len(res.Killed) > 0 {
		waitForExit(func() bool { return !PortStatusOf(port).Busy }, 3*time.Second)
	}
	res.Status = PortStatusOf(port)
	return res
}
