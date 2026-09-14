package winapi

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const (
	testOpenExisting             = 3
	testFileFlagBackupSemantics  = 0x02000000
	testFileFlagOpenReparsePoint = 0x00200000
)

// dumpReparseRaw 读取路径的重解析点原始缓冲区，供人工诊断偏移。
func dumpReparseRaw(t *testing.T, path string) []byte {
	t.Helper()

	p, err := syscall.UTF16PtrFromString(`\\?\` + path)
	if err != nil {
		t.Fatalf("UTF16PtrFromString 失败: %v", err)
	}
	h, err := syscall.CreateFile(
		p, 0,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, testOpenExisting,
		testFileFlagBackupSemantics|testFileFlagOpenReparsePoint, 0,
	)
	if err != nil {
		t.Fatalf("CreateFile 失败: %v", err)
	}
	defer syscall.CloseHandle(h)

	buf := make([]byte, 16*1024)
	var returned uint32
	if err := syscall.DeviceIoControl(h, FSCTL_GET_REPARSE_POINT,
		nil, 0, &buf[0], uint32(len(buf)), &returned, nil); err != nil {
		t.Fatalf("DeviceIoControl 失败: %v", err)
	}
	return buf[:returned]
}

// TestReparseLayoutOnJunction 用真实创建的目录联接（Junction）验证解析偏移。
//
// 这是本工具最关键的安全相关解析之一：读错目标路径会导致报告中展示错误的
// 链接信息；更严重的是后续里程碑基于路径操作时可能被误导。
// 因此必须用真实样本验证，而不是手写合成字节。
func TestReparseLayoutOnJunction(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "link")
	target := filepath.Join(base, "target")

	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("创建目标目录失败: %v", err)
	}

	// mklink /J 创建目录联接不需要管理员权限，正好符合本工具的使用前提。
	out, err := exec.Command("cmd", "/c", "mklink", "/J", src, target).CombinedOutput()
	if err != nil {
		t.Skipf("无法创建 Junction（跳过）：%v，输出：%s", err, strings.TrimSpace(string(out)))
	}

	raw := dumpReparseRaw(t, src)
	t.Logf("重解析点原始数据 %d 字节：", len(raw))
	limit := 112
	if len(raw) < limit {
		limit = len(raw)
	}
	for i := 0; i+16 <= limit; i += 16 {
		var words []string
		for j := 0; j < 16; j += 2 {
			words = append(words, formatHex16(binary.LittleEndian.Uint16(raw[i+j:i+j+2])))
		}
		t.Logf("  [%3d] %s", i, strings.Join(words, " "))
	}

	t.Logf("ReparseTag=0x%08X  ReparseDataLength=%d  Reserved=%d",
		binary.LittleEndian.Uint16(raw[4:6]), binary.LittleEndian.Uint16(raw[6:8]), 0)
	t.Logf("  tag=0x%08X dataLen=%d", binary.LittleEndian.Uint32(raw[0:4]),
		binary.LittleEndian.Uint16(raw[4:6]))
	t.Logf("  SubstituteOffset=%d SubstituteLen=%d PrintOffset=%d PrintLen=%d",
		binary.LittleEndian.Uint16(raw[8:10]), binary.LittleEndian.Uint16(raw[10:12]),
		binary.LittleEndian.Uint16(raw[12:14]), binary.LittleEndian.Uint16(raw[14:16]))

	info, err := ReadReparsePoint(`\\?\` + src)
	if err != nil {
		t.Fatalf("ReadReparsePoint 失败: %v", err)
	}
	t.Logf("解析结果：tag=%s substitute=%q print=%q target=%q",
		info.TagName(), info.Substitute, info.Print, info.Target())

	if info.Tag != IO_REPARSE_TAG_MOUNT_POINT {
		t.Errorf("tag 应为 MOUNT_POINT(0x%08X)，实际 0x%08X", IO_REPARSE_TAG_MOUNT_POINT, info.Tag)
	}
	got := strings.TrimRight(info.Target(), `\`)
	want := strings.TrimRight(target, `\`)
	if !strings.EqualFold(got, want) {
		t.Errorf("链接目标解析错误\n  期望: %s\n  实际: %s", want, got)
	}
}

func formatHex16(v uint16) string {
	const hex = "0123456789ABCDEF"
	return string([]byte{
		hex[(v>>12)&0xF], hex[(v>>8)&0xF], hex[(v>>4)&0xF], hex[v&0xF],
	})
}

// TestDumpSystemSymlink 转储系统自带的兼容性软链（如 C:\Users\All Users）。
//
// 这类路径是 IO_REPARSE_TAG_SYMLINK 而非 MOUNT_POINT，其 REPARSE_DATA_BUFFER
// 在 PathBuffer 之前多一个 DWORD Flags，因此 PathBuffer 偏移是 20 而不是 16。
// 用错偏移会读出错位的目标路径——这是必须用真实样本固定下来的回归测试。
func TestDumpSystemSymlink(t *testing.T) {
	candidates := []string{
		`C:\Users\All Users`,
		`C:\Users\Default User`,
		`C:\Documents and Settings`,
	}

	var found bool
	for _, p := range candidates {
		attrs, err := GetFileAttributes(`\\?\` + p)
		if err != nil || attrs&FILE_ATTRIBUTE_REPARSE_POINT == 0 {
			continue
		}
		found = true

		raw := dumpReparseRaw(t, p)
		tag := binary.LittleEndian.Uint32(raw[0:4])
		t.Logf("%s：tag=0x%08X (%s)", p, tag, ReparseInfo{Tag: tag}.TagName())
		t.Logf("  SubstituteOffset=%d SubstituteLen=%d PrintOffset=%d PrintLen=%d",
			binary.LittleEndian.Uint16(raw[8:10]), binary.LittleEndian.Uint16(raw[10:12]),
			binary.LittleEndian.Uint16(raw[12:14]), binary.LittleEndian.Uint16(raw[14:16]))
		t.Logf("  头部 24 字节：% X", raw[:24])

		info, err := ReadReparsePoint(`\\?\` + p)
		if err != nil {
			t.Errorf("  ReadReparsePoint 失败：%v", err)
			continue
		}
		t.Logf("  解析：substitute=%q print=%q target=%q", info.Substitute, info.Print, info.Target())

		if info.Target() == "" {
			t.Errorf("  %s 的目标路径解析为空", p)
		}
		if strings.Contains(info.Target(), "\x00") {
			t.Errorf("  %s 的目标路径含 NUL 字符，说明偏移错误", p)
		}
	}
	if !found {
		t.Skip("本机没有可用于验证的符号链接样本")
	}
}
