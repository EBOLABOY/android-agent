package brain

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	MemorySoulFile      = "SOUL.md"
	MemoryUserFile      = "MEMORY.md"
	MemoryLearningsFile = "LEARNINGS.md"
	maxMemoryFileBytes  = 16 * 1024
	maxPromptMemRunes   = 4500
)

type MemoryFile struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Content     string `json:"content"`
	Bytes       int    `json:"bytes"`
	UpdatedAt   string `json:"updated_at,omitempty"`
	AIWritable  bool   `json:"ai_writable"`
}

type memorySpec struct {
	Name        string
	Title       string
	Description string
	AIWritable  bool
	Default     string
}

var memorySpecs = []memorySpec{
	{
		Name:        MemorySoulFile,
		Title:       "人设与安全红线",
		Description: "定义 Agent 行为准则；仅用户可编辑，AI 不可擅自篡改。",
		AIWritable:  false,
		Default: `# SOUL.md — 人设与不可逾越的安全红线

- 我是运行在小米 13（Android 16 / HyperOS 3.0）上的端侧 AI 助手。
- **高风险操作红线**：凡涉及**付款、下单提交、转账、开通付费/自动续费会员、删除重要数据**，必须停在最终确认页，绝不替用户点击支付或确认按钮，并醒目提醒“需要人工确认”。
- **风控与隐私红线**：遇到滑块/验证码/人脸识别立即停止交由人工处理；不擅自下载第三方软件、不上传私密图片、不伪造或绕过系统权限。
- **执行优先级**：WebView 优先走 CDP，原生 App 优先用 phone_ui_tree + phone_tap_node，最后才调用 phone_screenshot 兜底。
`,
	},
	{
		Name:        MemoryUserFile,
		Title:       "长期偏好与习惯",
		Description: "记录常用 App 偏好、常用操作路线与用户明确交代的习惯。",
		AIWritable:  true,
		Default: `# MEMORY.md — 个人偏好与常用习惯

- 每日积分/签到任务优先使用内置 0-Token 引擎（daily_all / ms_rewards / jd_daily）。
- 美团点餐/逛店默认只浏览和加购，选好后停在购物车或确认订单页等待人工付款。
`,
	},
	{
		Name:        MemoryLearningsFile,
		Title:       "踩坑与纠错记录",
		Description: "沉淀 App 适配经验、页面异常与纠错策略，避免重复踩坑。",
		AIWritable:  true,
		Default: `# LEARNINGS.md — 实战踩坑与纠错记录

- 若某个原生页面返回空 UI 树（自绘/游戏/特殊 WebView），及时改用 phone_screenshot 或 cdp_list_targets。
- 仅在竖屏（ROTATION_0）直接注入 evdev 物理触控；横屏会由系统自动平滑回退到 cmd input。
`,
	},
}

// MemoryStore manages whitelisted Markdown memory files on disk and injects them into prompts.
type MemoryStore struct {
	mu  sync.RWMutex
	dir string
}

func NewMemoryStore(dir string) *MemoryStore {
	return &MemoryStore{dir: strings.TrimSpace(dir)}
}

func findMemorySpec(name string) (memorySpec, bool) {
	clean := strings.TrimSpace(name)
	for _, s := range memorySpecs {
		if strings.EqualFold(s.Name, clean) {
			return s, true
		}
	}
	return memorySpec{}, false
}

// EnsureDefaults creates the memory directory (0700) and seeds any missing Markdown files (0600).
func (m *MemoryStore) EnsureDefaults() error {
	if m == nil || m.dir == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return fmt.Errorf("create memory dir %s: %w", m.dir, err)
	}
	for _, spec := range memorySpecs {
		p := filepath.Join(m.dir, spec.Name)
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			if err := writeAtomicFile(p, []byte(spec.Default), 0o600); err != nil {
				return fmt.Errorf("seed %s: %w", spec.Name, err)
			}
		}
	}
	return nil
}

// List returns all three memory files with metadata and current content.
func (m *MemoryStore) List() []MemoryFile {
	out := make([]MemoryFile, 0, len(memorySpecs))
	if m == nil || m.dir == "" {
		return out
	}
	_ = m.EnsureDefaults()

	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, spec := range memorySpecs {
		p := filepath.Join(m.dir, spec.Name)
		content := spec.Default
		var updatedAt string
		if info, err := os.Stat(p); err == nil {
			updatedAt = info.ModTime().Format(time.RFC3339)
			if raw, rerr := os.ReadFile(p); rerr == nil {
				content = string(raw)
			}
		}
		out = append(out, MemoryFile{
			Name:        spec.Name,
			Title:       spec.Title,
			Description: spec.Description,
			Content:     content,
			Bytes:       len(content),
			UpdatedAt:   updatedAt,
			AIWritable:  spec.AIWritable,
		})
	}
	return out
}

// Read returns one whitelisted file or all files combined when name == "" or "all".
func (m *MemoryStore) Read(name string) (string, error) {
	if m == nil || m.dir == "" {
		return "", errors.New("memory store is not configured")
	}
	_ = m.EnsureDefaults()

	trim := strings.TrimSpace(name)
	if trim == "" || strings.EqualFold(trim, "all") {
		files := m.List()
		var sb strings.Builder
		for i, f := range files {
			if i > 0 {
				sb.WriteString("\n\n---\n\n")
			}
			sb.WriteString(strings.TrimSpace(f.Content))
		}
		return sb.String(), nil
	}

	spec, ok := findMemorySpec(trim)
	if !ok {
		return "", fmt.Errorf("invalid memory file %q (allowed: SOUL.md, MEMORY.md, LEARNINGS.md)", name)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	raw, err := os.ReadFile(filepath.Join(m.dir, spec.Name))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// Write replaces the full content of a whitelisted memory file (used by /ui editor).
func (m *MemoryStore) Write(name, content string) error {
	if m == nil || m.dir == "" {
		return errors.New("memory store is not configured")
	}
	spec, ok := findMemorySpec(name)
	if !ok {
		return fmt.Errorf("invalid memory file %q (allowed: SOUL.md, MEMORY.md, LEARNINGS.md)", name)
	}
	if len(content) > maxMemoryFileBytes {
		return fmt.Errorf("%s exceeds max size %d bytes", spec.Name, maxMemoryFileBytes)
	}
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}
	return writeAtomicFile(filepath.Join(m.dir, spec.Name), []byte(content), 0o600)
}

// Append adds a timestamped bullet point to MEMORY.md or LEARNINGS.md (AI cannot alter SOUL.md).
func (m *MemoryStore) Append(name, entry string) error {
	if m == nil || m.dir == "" {
		return errors.New("memory store is not configured")
	}
	spec, ok := findMemorySpec(name)
	if !ok {
		return fmt.Errorf("invalid memory file %q (allowed: MEMORY.md, LEARNINGS.md)", name)
	}
	if !spec.AIWritable {
		return fmt.Errorf("%s is protected and cannot be modified by the AI tool", spec.Name)
	}
	entry = strings.TrimSpace(entry)
	entry = strings.ReplaceAll(entry, "\r", "")
	if entry == "" {
		return errors.New("entry text is empty")
	}
	entry = strings.TrimPrefix(entry, "- ")

	_ = m.EnsureDefaults()

	m.mu.Lock()
	defer m.mu.Unlock()

	p := filepath.Join(m.dir, spec.Name)
	existing, _ := os.ReadFile(p)
	cur := strings.TrimRight(string(existing), "\n")
	line := fmt.Sprintf("- [%s] %s", time.Now().Format("2006-01-02 15:04"), entry)
	var updated string
	if cur == "" {
		updated = line + "\n"
	} else {
		updated = cur + "\n" + line + "\n"
	}
	if len(updated) > maxMemoryFileBytes {
		return fmt.Errorf("%s is full (%d bytes limit); trim old entries in /ui Memory drawer", spec.Name, maxMemoryFileBytes)
	}
	return writeAtomicFile(p, []byte(updated), 0o600)
}

// BuildPromptSection formats SOUL.md, MEMORY.md, and LEARNINGS.md for per-turn System Prompt injection.
func (m *MemoryStore) BuildPromptSection() string {
	if m == nil || m.dir == "" {
		return ""
	}
	files := m.List()
	if len(files) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("【端侧持久化记忆与不可逾越准则】")
	for _, f := range files {
		c := strings.TrimSpace(f.Content)
		if c == "" {
			continue
		}
		sb.WriteString("\n\n### ")
		sb.WriteString(f.Name)
		sb.WriteString("\n")
		sb.WriteString(c)
	}
	out := sb.String()
	if utf8.RuneCountInString(out) > maxPromptMemRunes {
		r := []rune(out)
		out = string(r[:maxPromptMemRunes]) + "\n...(已截断)"
	}
	return out
}

func writeAtomicFile(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	_ = os.Chmod(tmp, perm)
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
