package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/config"
)

const (
	DefaultLogMaxSize = 10 * 1024 * 1024 // 10 MB
	ServiceName       = "echo-client"
	LegacyServiceName = "cc-connect"
)

type Config struct {
	BinaryPath string
	WorkDir    string
	LogFile    string
	LogMaxSize int64
	DataDir    string
	ConfigPath string
	EnvPATH    string // capture user's PATH so agents are accessible
}

type Status struct {
	Installed bool
	Running   bool
	PID       int
	Platform  string // "systemd", "launchd", "schtasks"
}

type Manager interface {
	Install(cfg Config) error
	Uninstall() error
	Start() error
	Stop() error
	Restart() error
	Status() (*Status, error)
	Platform() string
}

// NewManager returns a platform-specific daemon manager.
func NewManager() (Manager, error) {
	return newPlatformManager()
}

func DefaultLogFile() string {
	if meta, err := LoadMeta(); err == nil && strings.TrimSpace(meta.LogFile) != "" {
		return meta.LogFile
	}
	return filepath.Join(DefaultDataDir(), "logs", "echo-client.log")
}

func DefaultDataDir() string {
	if meta, err := LoadMeta(); err == nil {
		if strings.TrimSpace(meta.DataDir) != "" {
			return meta.DataDir
		}
		if dir := daemonStateMetaDir(); dir != "" {
			return dir
		}
	}
	if dataDir, ok := config.ResolveDefaultHomeDataDirFromHomeConfig(); ok {
		return dataDir
	}
	return config.ResolveDefaultHomeDataDir()
}

// ── Metadata ────────────────────────────────────────────────
// Stored at ~/.echo-client/daemon.json by default (or legacy ~/.cc-connect/daemon.json)
// so that `logs`, `status`,
// etc. can locate the log file without parsing service definitions.

type Meta struct {
	LogFile     string `json:"log_file"`
	LogMaxSize  int64  `json:"log_max_size"`
	WorkDir     string `json:"work_dir"`
	BinaryPath  string `json:"binary_path"`
	DataDir     string `json:"data_dir"`
	ConfigPath  string `json:"config_path"`
	InstalledAt string `json:"installed_at"`
}

func metaPath() string {
	return filepath.Join(defaultMetaDir(), "daemon.json")
}

func daemonStateMetaDir() string {
	preferredDir, legacyDir, ok := configHomeDirs()
	if !ok {
		return ""
	}

	preferredMeta := filepath.Join(preferredDir, "daemon.json")
	legacyMeta := filepath.Join(legacyDir, "daemon.json")

	switch {
	case metaFileIsUsable(preferredMeta):
		return preferredDir
	case metaFileIsUsable(legacyMeta):
		return legacyDir
	default:
		return ""
	}
}

func defaultMetaDir() string {
	if dir := daemonStateMetaDir(); dir != "" {
		return dir
	}
	preferredDir, legacyDir, ok := configHomeDirs()
	if !ok {
		return config.DefaultAppHomeDirName
	}
	preferredConfig := filepath.Join(preferredDir, "config.toml")
	legacyConfig := filepath.Join(legacyDir, "config.toml")
	switch {
	case fileExists(preferredConfig):
		return preferredDir
	case fileExists(legacyConfig):
		return legacyDir
	case dirExists(preferredDir):
		return preferredDir
	case dirExists(legacyDir):
		return legacyDir
	default:
		return preferredDir
	}
}

func metaPathForConfig(configPath string) string {
	dir := defaultMetaDir()
	preferredDir, legacyDir, ok := configHomeDirs()
	if ok {
		preferredConfig := filepath.Join(preferredDir, "config.toml")
		legacyConfig := filepath.Join(legacyDir, "config.toml")
		switch {
		case samePath(configPath, preferredConfig), samePath(filepath.Dir(configPath), preferredDir):
			dir = preferredDir
		case samePath(configPath, legacyConfig), samePath(filepath.Dir(configPath), legacyDir):
			dir = legacyDir
		}
	}
	return filepath.Join(dir, "daemon.json")
}

func configHomeDirs() (preferred string, legacy string, ok bool) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", "", false
	}
	return filepath.Join(home, config.DefaultAppHomeDirName), filepath.Join(home, config.LegacyAppHomeDirName), true
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func metaFileIsUsable(path string) bool {
	if !fileExists(path) {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var meta Meta
	return json.Unmarshal(data, &meta) == nil
}

func SaveMeta(m *Meta) error {
	targetPath := metaPath()
	if m != nil && strings.TrimSpace(m.ConfigPath) != "" {
		targetPath = metaPathForConfig(m.ConfigPath)
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(targetPath, data, 0644)
}

func LoadMeta() (*Meta, error) {
	data, err := os.ReadFile(metaPath())
	if err != nil {
		return nil, err
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func RemoveMeta() {
	_ = os.Remove(metaPath())
}

func NowISO() string {
	return time.Now().Format(time.RFC3339)
}

func Resolve(cfg *Config) error {
	if cfg.BinaryPath == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("cannot detect binary path: %w", err)
		}
		real, err := filepath.EvalSymlinks(exe)
		if err == nil {
			exe = real
		}
		cfg.BinaryPath = exe
	}
	if cfg.WorkDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("cannot detect working directory: %w", err)
		}
		cfg.WorkDir = wd
	}
	if cfg.LogFile == "" {
		logDataDir := strings.TrimSpace(cfg.DataDir)
		if logDataDir == "" {
			logDataDir = DefaultDataDir()
		}
		cfg.LogFile = filepath.Join(logDataDir, "logs", "echo-client.log")
	}
	if cfg.LogMaxSize <= 0 {
		cfg.LogMaxSize = DefaultLogMaxSize
	}
	if cfg.EnvPATH == "" {
		cfg.EnvPATH = os.Getenv("PATH")
	}
	return nil
}

func samePath(a, b string) bool {
	if strings.TrimSpace(a) == "" || strings.TrimSpace(b) == "" {
		return false
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return filepath.Clean(absA) == filepath.Clean(absB)
}
