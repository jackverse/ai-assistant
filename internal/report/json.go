package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"winclean/internal/model"
)

// MarshalJSON 把扫描结果序列化为带缩进的 JSON。
//
// 关闭 HTML 转义：默认的 encoding/json 会把 < > & 转成 \u003c 之类，
// 对路径而言虽然语义无损但可读性很差，而本工具的 JSON 是给人审阅和 diff 的。
func MarshalJSON(res *model.ScanResult) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// WriteJSON 把扫描结果写入文件。
//
// 先写临时文件再 rename：避免在长时间扫描后写到一半失败，
// 留下一个半截的 scan.json 被后续命令当成有效输入。
func WriteJSON(path string, res *model.ScanResult) error {
	data, err := MarshalJSON(res)
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
		}
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("替换 %s 失败: %w", path, err)
	}
	return nil
}

// ReadJSON 读取并解析 scan.json。
func ReadJSON(path string) (*model.ScanResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	var res model.ScanResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("解析 %s 失败（文件可能已损坏或被截断）: %w", path, err)
	}
	if res.SchemaVersion == "" {
		return nil, fmt.Errorf("%s 不是有效的 winclean 扫描结果（缺少 schema_version）", path)
	}
	return &res, nil
}
