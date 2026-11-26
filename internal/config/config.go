package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type LLMConfig struct {
	Provider   string `json:"provider"`    // "openai" (OpenAI/DeepSeek/OpenRouter/OneAPI) or "gemini"
	BaseURL    string `json:"base_url"`    // e.g. "https://api.deepseek.com/v1" or "https://generativelanguage.googleapis.com/v1beta"
	APIKey     string `json:"api_key"`
	Model      string `json:"model"`
	MaxSteps   int    `json:"max_steps"`
	TimeoutSec int    `json:"timeout_sec"`
}

type ScheduleEntry struct {
	ID          string `json:"id"`
	Job         string `json:"job"`           // "daily_all" | "ms_rewards" | "jd_daily"
	Enabled     bool   `json:"enabled"`
	WindowStart string `json:"window_start"`  // "HH:MM" local 24h time
	WindowEnd   string `json:"window_end"`    // "HH:MM" local 24h time
	DeferOnGame bool   `json:"defer_on_game"` // true: defer if guarded game foreground; false: skip today
}

type ScheduleConfig struct {
	Enabled bool            `json:"enabled"`
	Entries []ScheduleEntry `json:"entries,omitempty"`
}

type Config struct {
	ListenAddr            string         `json:"listen_addr"`
	BearerToken           string         `json:"bearer_token"`
	ScreenshotWidth       int            `json:"screenshot_width"`
	ScreenshotJPEGQuality int            `json:"screenshot_jpeg_quality"`
	GuardPackages         []string       `json:"guard_packages"`
	GuardPollIntervalMs   int            `json:"guard_poll_interval_ms"`
	GuardResumeGraceMs    int            `json:"guard_resume_grace_ms"`
	CDPSocket             string         `json:"cdp_socket"`
	SysSocket             string         `json:"sys_socket"`
	ClipHelperPath        string         `json:"clip_helper_path"`
	PauseFlagPath         string         `json:"pause_flag_path"`
	ShellTimeoutMs        int            `json:"shell_timeout_ms"`
	MemoryDir             string         `json:"memory_dir,omitempty"`
	LLM                   LLMConfig      `json:"llm,omitempty"`
	Schedule              ScheduleConfig `json:"schedule,omitempty"`
}

func Default() *Config {
	return &Config{
		ListenAddr:            "0.0.0.0:8765",
		BearerToken:           "",
		ScreenshotWidth:       720,
		ScreenshotJPEGQuality: 70,
		GuardPackages:         []string{"com.tencent.tmgp.sgame"},
		GuardPollIntervalMs:   2000,
		GuardResumeGraceMs:    3000,
		CDPSocket:             "chrome_devtools_remote",
		SysSocket:             "agent_sys",
		ClipHelperPath:        "/data/local/tmp/android_agent_cliphelper.jar",
		PauseFlagPath:         "/data/adb/android_agent/paused",
		ShellTimeoutMs:        10000,
		MemoryDir:             "/data/adb/android_agent/memory",
		LLM: LLMConfig{
			Provider:   "openai",
			MaxSteps:   20,
			TimeoutSec: 60,
		},
		Schedule: ScheduleConfig{
			Enabled: false,
			Entries: []ScheduleEntry{
				{
					ID:          "daily-all",
					Job:         "daily_all",
					Enabled:     true,
					WindowStart: "08:15",
					WindowEnd:   "09:30",
					DeferOnGame: true,
				},
			},
		},
	}
}

// MaskSecret returns a safe masked preview (e.g. "sk-a...wxyz") for HTTP APIs and logs.
func MaskSecret(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + "..." + s[len(s)-4:]
}

func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Load loads config from path (creating it with a generated token if createIfMissing is true),
// then applies environment variable overrides.
func Load(path string, createIfMissing bool) (*Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && createIfMissing {
				tok, genErr := GenerateToken()
				if genErr != nil {
					return nil, fmt.Errorf("generate token: %w", genErr)
				}
				cfg.BearerToken = tok
				if err := Save(path, cfg); err != nil {
					return nil, fmt.Errorf("save default config: %w", err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("read config %s: %w", path, err)
			}
		} else {
			if err := json.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("parse config %s: %w", path, err)
			}
		}
	}

	applyEnv(cfg)
	applyDefaults(cfg)

	if strings.TrimSpace(cfg.BearerToken) == "" {
		return nil, errors.New("bearer_token is required (set in config.json or AGENT_BEARER_TOKEN)")
	}
	return cfg, nil
}

func Save(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}

func applyEnv(cfg *Config) {
	if v := strings.TrimSpace(os.Getenv("AGENT_LISTEN_ADDR")); v != "" {
		cfg.ListenAddr = v
	}
	if v := strings.TrimSpace(os.Getenv("AGENT_BEARER_TOKEN")); v != "" {
		cfg.BearerToken = v
	}
	if v := strings.TrimSpace(os.Getenv("AGENT_SCREENSHOT_WIDTH")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.ScreenshotWidth = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("AGENT_SCREENSHOT_QUALITY")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 100 {
			cfg.ScreenshotJPEGQuality = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("AGENT_GUARD_PACKAGES")); v != "" {
		parts := strings.Split(v, ",")
		var out []string
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		cfg.GuardPackages = out
	}
	if v := strings.TrimSpace(os.Getenv("AGENT_CDP_SOCKET")); v != "" {
		cfg.CDPSocket = v
	}
	if v := strings.TrimSpace(os.Getenv("AGENT_CLIP_HELPER_PATH")); v != "" {
		cfg.ClipHelperPath = v
	}
	if v := strings.TrimSpace(os.Getenv("AGENT_MEMORY_DIR")); v != "" {
		cfg.MemoryDir = v
	}
	if v := strings.TrimSpace(os.Getenv("AGENT_LLM_PROVIDER")); v != "" {
		cfg.LLM.Provider = strings.ToLower(v)
	}
	if v := strings.TrimSpace(os.Getenv("AGENT_LLM_BASE_URL")); v != "" {
		cfg.LLM.BaseURL = v
	}
	if v := strings.TrimSpace(os.Getenv("AGENT_LLM_API_KEY")); v != "" {
		cfg.LLM.APIKey = v
	}
	if v := strings.TrimSpace(os.Getenv("AGENT_LLM_MODEL")); v != "" {
		cfg.LLM.Model = v
	}
}

func applyDefaults(cfg *Config) {
	def := Default()
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = def.ListenAddr
	}
	if cfg.ScreenshotWidth <= 0 {
		cfg.ScreenshotWidth = def.ScreenshotWidth
	}
	if cfg.ScreenshotJPEGQuality <= 0 || cfg.ScreenshotJPEGQuality > 100 {
		cfg.ScreenshotJPEGQuality = def.ScreenshotJPEGQuality
	}
	if cfg.GuardPollIntervalMs <= 0 {
		cfg.GuardPollIntervalMs = def.GuardPollIntervalMs
	}
	if cfg.GuardResumeGraceMs < 0 {
		cfg.GuardResumeGraceMs = def.GuardResumeGraceMs
	}
	if cfg.CDPSocket == "" {
		cfg.CDPSocket = def.CDPSocket
	}
	if cfg.SysSocket == "" {
		cfg.SysSocket = def.SysSocket
	}
	if cfg.ClipHelperPath == "" {
		cfg.ClipHelperPath = def.ClipHelperPath
	}
	if cfg.ShellTimeoutMs <= 0 {
		cfg.ShellTimeoutMs = def.ShellTimeoutMs
	}
	if strings.TrimSpace(cfg.MemoryDir) == "" {
		cfg.MemoryDir = def.MemoryDir
	}
	if len(cfg.Schedule.Entries) == 0 {
		cfg.Schedule.Entries = append([]ScheduleEntry(nil), def.Schedule.Entries...)
	}
	if strings.TrimSpace(cfg.LLM.Provider) == "" {
		cfg.LLM.Provider = def.LLM.Provider
	} else {
		cfg.LLM.Provider = strings.ToLower(strings.TrimSpace(cfg.LLM.Provider))
	}
	if cfg.LLM.MaxSteps <= 0 {
		cfg.LLM.MaxSteps = def.LLM.MaxSteps
	}
	if cfg.LLM.TimeoutSec <= 0 {
		cfg.LLM.TimeoutSec = def.LLM.TimeoutSec
	}
}
