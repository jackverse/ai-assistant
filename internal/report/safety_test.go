package report

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSafeVerdictBlocksUserTempFolders 验证用户自建的 temp 目录不会被误判。
//
// 这是实测发现的真实风险：原规则用关键词 `\temp` 匹配，
// D:\work\temp 这类用户目录也会被判为「可安全清理」。
func TestSafeVerdictBlocksUserTempFolders(t *testing.T) {
	// 不在 LOCALAPPDATA 下的 temp 目录，必须【不】进绿色区
	for _, p := range []string{
		`D:\work\temp`,
		`D:\projects\build\temp`,
		`E:\backup\temp`,
	} {
		v := safeCleanVerdict(p, nil)
		if v.Safe {
			t.Errorf("%s 不该被判为可安全清理", p)
		}
	}
}

// TestSafeVerdictBlocksUpdaterProjects 验证代码库里的 xxx-updater 项目不会被误判。
//
// 原规则用 `-updater` 子串匹配，会命中 D:\code\my-updater 这类项目目录。
func TestSafeVerdictBlocksUpdaterProjects(t *testing.T) {
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		t.Skip("无 LOCALAPPDATA，跳过")
	}
	// 即便在 LOCALAPPDATA 下，只要落在代码根内也必须被拦
	codeRoot := filepath.Join(local, "my-updater")
	v := safeCleanVerdict(codeRoot, []string{filepath.Join(local)})
	if v.Safe {
		t.Error("位于代码根目录下的路径不该被判为可安全清理")
	}
	if v.Block == "" {
		t.Error("被拦下时必须给出原因")
	}

	// 深层的 -updater 目录（非 LocalAppData 一级子目录）也不该被判安全
	deep := filepath.Join(local, "SomeApp", "inner-updater")
	if v := safeCleanVerdict(deep, nil); v.Safe {
		t.Error("非一级子目录的 -updater 不该被判为可安全清理")
	}
}

// TestSafeVerdictAllowsRealTemp 验证真正的临时目录仍能进绿色区。
func TestSafeVerdictAllowsRealTemp(t *testing.T) {
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		t.Skip("无 LOCALAPPDATA，跳过")
	}
	// 注意：这些目录可能不存在，safeCleanVerdict 只看路径不查存在性
	for _, p := range []string{
		filepath.Join(local, "Temp"),
		filepath.Join(local, "Temp", "some", "nested"),
		filepath.Join(local, "CrashDumps"),
	} {
		v := safeCleanVerdict(p, nil)
		if !v.Safe {
			t.Errorf("%s 应被判为可安全清理（实际被拦：%s）", p, v.Block)
		}
		if v.Reason == "" {
			t.Errorf("%s 进绿色区时必须给出人话原因", p)
		}
	}
}

// TestLooksLikeProject 验证项目目录识别。
//
// 含项目特征文件的目录绝不能进绿色区——那是用户的工作成果。
func TestLooksLikeProject(t *testing.T) {
	dir := t.TempDir()
	if looksLikeProject(dir) {
		t.Error("空目录不该被识别为项目")
	}

	for _, marker := range []string{".git", "pom.xml", "package.json", "go.mod", "README.md"} {
		d := filepath.Join(dir, marker)
		if marker == ".git" {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.WriteFile(d, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if !looksLikeProject(dir) {
			t.Errorf("含 %s 的目录应被识别为项目目录", marker)
		}
	}
}

// TestSafeVerdictBlocksUserData 验证用户数据不会被判为可清理。
func TestSafeVerdictBlocksUserData(t *testing.T) {
	for _, p := range []string{
		`C:\Users\x\Documents\temp`,
		`C:\Users\x\Desktop\temp`,
		`C:\Users\x\OneDrive\temp`,
		`C:\Users\x\AppData\Roaming\Tencent\temp`,
	} {
		v := safeCleanVerdict(p, nil)
		if v.Safe {
			t.Errorf("用户数据路径 %s 不该被判为可安全清理", p)
		}
	}
}

// TestSafeVerdictBlocksDriveRoot 验证盘根不会被判为可清理。
func TestSafeVerdictBlocksDriveRoot(t *testing.T) {
	if v := safeCleanVerdict(`D:\`, nil); v.Safe {
		t.Error("盘根不该被判为可安全清理")
	}
}
