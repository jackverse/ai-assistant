package migrate

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"winclean/internal/config"
)

// Journal 是迁移的记账文件（JSONL，每行一条事件）。
//
// 为什么必须落盘且逐步 fsync（docs/design/09 §7）：迁移几十 GB 时断电是
// 真实可能。journal 是 undo 的唯一输入，只在内存里攒着等于没有回滚能力。
type Journal struct {
	mu   sync.Mutex
	path string
	f    *os.File
}

// Entry 是一条事件记录。
type Entry struct {
	Time time.Time `json:"time"`
	Kind string    `json:"kind"` // begin / item_copy_done / item_done / item_failed / done / undone
	// item_done / item_failed / undone 携带的具体内容
	Item ItemRecord `json:"item,omitempty"`
}

// ItemRecord 是一次成功迁移的完整可回滚描述。
type ItemRecord struct {
	ID       string `json:"id"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	Backup   string `json:"backup"`
	Bytes    int64  `json:"bytes"`
	Files    int64  `json:"files"`
	EnvVar   string `json:"env_var,omitempty"`
	EnvOld   string `json:"env_old,omitempty"`   // 空且 EnvOldSet=false 表示原本不存在
	EnvOldSet bool  `json:"env_old_set"`
	Done     bool   `json:"done"`
	Undone   bool   `json:"undone"`
	Purged   bool   `json:"purged"`
}

// NewJournal 在配置目录下创建 journal 文件（%LOCALAPPDATA%\winclean\journal）。
func NewJournal(id string) (*Journal, string, error) {
	dir, err := journalDir()
	if err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", fmt.Errorf("创建 journal 目录失败: %w", err)
	}
	p := filepath.Join(dir, id+".jsonl")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, "", fmt.Errorf("创建 journal 失败: %w", err)
	}
	return &Journal{path: p, f: f}, p, nil
}

func journalDir() (string, error) {
	cfgPath, err := config.Path()
	if err != nil {
		return "", err
	}
	// 配置文件在 %LOCALAPPDATA%\winclean\config.yaml，journal 与之同根
	return filepath.Join(filepath.Dir(cfgPath), "journal"), nil
}

// Write 追加一条事件并立即 fsync。
func (j *Journal) Write(e Entry) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return fmt.Errorf("journal 已关闭")
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := j.f.Write(append(b, '\n')); err != nil {
		return err
	}
	return j.f.Sync()
}

// Close 收尾。
func (j *Journal) Close() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f != nil {
		j.f.Close()
		j.f = nil
	}
}

// FileRecord 是一个 journal 文件读回后的视图（给撤销界面用）。
type FileRecord struct {
	ID      string       `json:"id"`
	Path    string       `json:"path"`
	Time    time.Time    `json:"time"`
	Items   []ItemRecord `json:"items"`
	Err     string       `json:"err,omitempty"`
}

// ListJournals 列出全部 journal（新的在前）。
func ListJournals() []FileRecord {
	dir, err := journalDir()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []FileRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		rec := readJournal(p)
		rec.ID = strings.TrimSuffix(e.Name(), ".jsonl")
		rec.Path = p
		out = append(out, rec)
	}
	// 新的在前（ID 内含时间戳，字典序即可）
	for i := 0; i < len(out); i++ {
		for k := i + 1; k < len(out); k++ {
			if out[k].ID > out[i].ID {
				out[i], out[k] = out[k], out[i]
			}
		}
	}
	return out
}

// readJournal 逐行解析；损坏行跳过（崩溃可能留下半行）。
func readJournal(path string) FileRecord {
	rec := FileRecord{Path: path}
	f, err := os.Open(path)
	if err != nil {
		rec.Err = err.Error()
		return rec
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			rec.Err = "存在无法解析的记录行（可能因崩溃中断）"
			continue
		}
		if rec.Time.IsZero() {
			rec.Time = e.Time
		}
		switch e.Kind {
		case "item_done":
			// 以最后一次 item_done 为准（同一 ID 可能被重试过）
			upsertItem(&rec, e.Item)
		case "item_undone":
			if it := findItem(&rec, e.Item.ID); it != nil {
				it.Undone = true
			}
		case "item_purged":
			if it := findItem(&rec, e.Item.ID); it != nil {
				it.Purged = true
			}
		}
	}
	return rec
}

func findItem(rec *FileRecord, id string) *ItemRecord {
	for i := range rec.Items {
		if rec.Items[i].ID == id {
			return &rec.Items[i]
		}
	}
	return nil
}

func upsertItem(rec *FileRecord, it ItemRecord) {
	if old := findItem(rec, it.ID); old != nil {
		*old = it
		return
	}
	rec.Items = append(rec.Items, it)
}
