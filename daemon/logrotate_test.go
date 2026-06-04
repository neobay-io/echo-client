package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingWriter(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")

	maxSize := int64(500) // 500 bytes
	w, err := NewRotatingWriter(logPath, maxSize)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer func() {
		_ = w.Close()
	}()

	line := strings.Repeat("A", 100) + "\n" // 101 bytes

	for i := 0; i < 10; i++ {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatalf("Write #%d: %v", i, err)
		}
	}

	// After 10 writes of 101 bytes = 1010 bytes, rotation should have occurred.
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("Stat main: %v", err)
	}
	if info.Size() > maxSize+200 {
		t.Errorf("main log too large: %d bytes (max %d)", info.Size(), maxSize)
	}

	backupPath := logPath + ".1"
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatalf("backup file should exist: %v", err)
	}

	t.Logf("main: %d bytes, backup exists", info.Size())
}

func TestMetaSaveLoad(t *testing.T) {
	origHome := os.Getenv("HOME")
	dir := t.TempDir()
	if err := os.Setenv("HOME", dir); err != nil {
		t.Fatalf("Setenv HOME: %v", err)
	}
	defer func() {
		if err := os.Setenv("HOME", origHome); err != nil {
			t.Fatalf("restore HOME: %v", err)
		}
	}()

	m := &Meta{
		LogFile:     "/tmp/test.log",
		LogMaxSize:  1024,
		WorkDir:     "/tmp",
		BinaryPath:  "/usr/local/bin/echo-client",
		DataDir:     "/tmp/echo-data",
		ConfigPath:  "/tmp/echo-config/config.toml",
		InstalledAt: NowISO(),
	}

	if err := SaveMeta(m); err != nil {
		t.Fatalf("SaveMeta: %v", err)
	}

	loaded, err := LoadMeta()
	if err != nil {
		t.Fatalf("LoadMeta: %v", err)
	}

	if loaded.LogFile != m.LogFile {
		t.Errorf("LogFile mismatch: %s != %s", loaded.LogFile, m.LogFile)
	}
	if loaded.WorkDir != m.WorkDir {
		t.Errorf("WorkDir mismatch: %s != %s", loaded.WorkDir, m.WorkDir)
	}
	if loaded.DataDir != m.DataDir {
		t.Errorf("DataDir mismatch: %s != %s", loaded.DataDir, m.DataDir)
	}
	if loaded.ConfigPath != m.ConfigPath {
		t.Errorf("ConfigPath mismatch: %s != %s", loaded.ConfigPath, m.ConfigPath)
	}
}

func TestDefaultLogFileUsesEchoClientName(t *testing.T) {
	origHome := os.Getenv("HOME")
	dir := t.TempDir()
	if err := os.Setenv("HOME", dir); err != nil {
		t.Fatalf("Setenv HOME: %v", err)
	}
	defer func() {
		if err := os.Setenv("HOME", origHome); err != nil {
			t.Fatalf("restore HOME: %v", err)
		}
	}()

	if got := DefaultLogFile(); got != filepath.Join(dir, ".echo-client", "logs", "echo-client.log") {
		t.Fatalf("DefaultLogFile() = %q", got)
	}
}

func TestDefaultLogFileFallsBackToLegacyHomeDir(t *testing.T) {
	origHome := os.Getenv("HOME")
	dir := t.TempDir()
	if err := os.Setenv("HOME", dir); err != nil {
		t.Fatalf("Setenv HOME: %v", err)
	}
	defer func() {
		if err := os.Setenv("HOME", origHome); err != nil {
			t.Fatalf("restore HOME: %v", err)
		}
	}()

	legacyDir := filepath.Join(dir, ".cc-connect")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatalf("MkdirAll legacy: %v", err)
	}

	if got := DefaultLogFile(); got != filepath.Join(legacyDir, "logs", "echo-client.log") {
		t.Fatalf("DefaultLogFile() = %q", got)
	}
}

func TestLoadMetaPrefersExistingLegacyDaemonStateWhenPreferredDirAlsoExists(t *testing.T) {
	origHome := os.Getenv("HOME")
	dir := t.TempDir()
	if err := os.Setenv("HOME", dir); err != nil {
		t.Fatalf("Setenv HOME: %v", err)
	}
	defer func() {
		if err := os.Setenv("HOME", origHome); err != nil {
			t.Fatalf("restore HOME: %v", err)
		}
	}()

	if err := os.MkdirAll(filepath.Join(dir, ".echo-client"), 0o755); err != nil {
		t.Fatalf("MkdirAll preferred dir: %v", err)
	}

	legacyMetaPath := filepath.Join(dir, ".cc-connect", "daemon.json")
	if err := os.MkdirAll(filepath.Dir(legacyMetaPath), 0o755); err != nil {
		t.Fatalf("MkdirAll legacy meta dir: %v", err)
	}
	if err := os.WriteFile(legacyMetaPath, []byte(`{"log_file":"/tmp/legacy.log","work_dir":"/tmp/legacy","binary_path":"/usr/local/bin/echo-client","installed_at":"2026-01-01T00:00:00Z"}`), 0o644); err != nil {
		t.Fatalf("WriteFile legacy meta: %v", err)
	}

	loaded, err := LoadMeta()
	if err != nil {
		t.Fatalf("LoadMeta: %v", err)
	}
	if loaded.LogFile != "/tmp/legacy.log" {
		t.Fatalf("loaded.LogFile = %q, want legacy log path", loaded.LogFile)
	}
	if got := DefaultDataDir(); got != filepath.Join(dir, ".cc-connect") {
		t.Fatalf("DefaultDataDir() = %q, want legacy data dir", got)
	}
	if got := DefaultLogFile(); got != "/tmp/legacy.log" {
		t.Fatalf("DefaultLogFile() = %q, want metadata log file", got)
	}
}

func TestDefaultLogFileIgnoresCorruptLegacyDaemonState(t *testing.T) {
	origHome := os.Getenv("HOME")
	dir := t.TempDir()
	if err := os.Setenv("HOME", dir); err != nil {
		t.Fatalf("Setenv HOME: %v", err)
	}
	defer func() {
		if err := os.Setenv("HOME", origHome); err != nil {
			t.Fatalf("restore HOME: %v", err)
		}
	}()

	preferredMetaPath := filepath.Join(dir, ".echo-client", "daemon.json")
	if err := os.MkdirAll(filepath.Dir(preferredMetaPath), 0o755); err != nil {
		t.Fatalf("MkdirAll preferred meta dir: %v", err)
	}
	if err := os.WriteFile(preferredMetaPath, []byte(`{"log_file":"/tmp/preferred.log","work_dir":"/tmp/preferred","binary_path":"/usr/local/bin/echo-client","installed_at":"2026-01-01T00:00:00Z"}`), 0o644); err != nil {
		t.Fatalf("WriteFile preferred meta: %v", err)
	}

	legacyMetaPath := filepath.Join(dir, ".cc-connect", "daemon.json")
	if err := os.MkdirAll(filepath.Dir(legacyMetaPath), 0o755); err != nil {
		t.Fatalf("MkdirAll legacy meta dir: %v", err)
	}
	if err := os.WriteFile(legacyMetaPath, []byte("{"), 0o644); err != nil {
		t.Fatalf("WriteFile corrupt legacy meta: %v", err)
	}

	if got := DefaultDataDir(); got != filepath.Join(dir, ".echo-client") {
		t.Fatalf("DefaultDataDir() = %q, want preferred data dir", got)
	}
	if got := DefaultLogFile(); got != "/tmp/preferred.log" {
		t.Fatalf("DefaultLogFile() = %q, want metadata log file", got)
	}
}

func TestDefaultDataDirUsesConfiguredDaemonMetadataPath(t *testing.T) {
	origHome := os.Getenv("HOME")
	dir := t.TempDir()
	if err := os.Setenv("HOME", dir); err != nil {
		t.Fatalf("Setenv HOME: %v", err)
	}
	defer func() {
		if err := os.Setenv("HOME", origHome); err != nil {
			t.Fatalf("restore HOME: %v", err)
		}
	}()

	metaPath := filepath.Join(dir, ".echo-client", "daemon.json")
	if err := os.MkdirAll(filepath.Dir(metaPath), 0o755); err != nil {
		t.Fatalf("MkdirAll meta dir: %v", err)
	}
	customDataDir := filepath.Join(dir, "custom-data")
	content := `{"data_dir":"` + customDataDir + `","log_file":"` + filepath.Join(customDataDir, "logs", "echo-client.log") + `","work_dir":"/tmp","binary_path":"/usr/local/bin/echo-client","installed_at":"2026-01-01T00:00:00Z"}`
	if err := os.WriteFile(metaPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile meta: %v", err)
	}

	if got := DefaultDataDir(); got != customDataDir {
		t.Fatalf("DefaultDataDir() = %q, want %q", got, customDataDir)
	}
	if got := DefaultLogFile(); got != filepath.Join(customDataDir, "logs", "echo-client.log") {
		t.Fatalf("DefaultLogFile() = %q", got)
	}
}

func TestLoadMetaPrefersMostRecentlyInstalledRecordAcrossDirs(t *testing.T) {
	origHome := os.Getenv("HOME")
	dir := t.TempDir()
	if err := os.Setenv("HOME", dir); err != nil {
		t.Fatalf("Setenv HOME: %v", err)
	}
	defer func() {
		if err := os.Setenv("HOME", origHome); err != nil {
			t.Fatalf("restore HOME: %v", err)
		}
	}()

	preferredMetaPath := filepath.Join(dir, ".echo-client", "daemon.json")
	if err := os.MkdirAll(filepath.Dir(preferredMetaPath), 0o755); err != nil {
		t.Fatalf("MkdirAll preferred meta dir: %v", err)
	}
	if err := os.WriteFile(preferredMetaPath, []byte(`{"data_dir":"/tmp/preferred-data","config_path":"/tmp/preferred.toml","installed_at":"2026-06-01T00:00:00Z"}`), 0o644); err != nil {
		t.Fatalf("WriteFile preferred meta: %v", err)
	}

	legacyMetaPath := filepath.Join(dir, ".cc-connect", "daemon.json")
	if err := os.MkdirAll(filepath.Dir(legacyMetaPath), 0o755); err != nil {
		t.Fatalf("MkdirAll legacy meta dir: %v", err)
	}
	if err := os.WriteFile(legacyMetaPath, []byte(`{"data_dir":"/tmp/legacy-data","config_path":"/tmp/legacy.toml","installed_at":"2026-06-02T00:00:00Z"}`), 0o644); err != nil {
		t.Fatalf("WriteFile legacy meta: %v", err)
	}

	meta, err := LoadMeta()
	if err != nil {
		t.Fatalf("LoadMeta: %v", err)
	}
	if meta.DataDir != "/tmp/legacy-data" {
		t.Fatalf("meta.DataDir = %q, want latest legacy record", meta.DataDir)
	}
}

func TestRemoveMetaRemovesBothPreferredAndLegacyFiles(t *testing.T) {
	origHome := os.Getenv("HOME")
	dir := t.TempDir()
	if err := os.Setenv("HOME", dir); err != nil {
		t.Fatalf("Setenv HOME: %v", err)
	}
	defer func() {
		if err := os.Setenv("HOME", origHome); err != nil {
			t.Fatalf("restore HOME: %v", err)
		}
	}()

	paths := []string{
		filepath.Join(dir, ".echo-client", "daemon.json"),
		filepath.Join(dir, ".cc-connect", "daemon.json"),
	}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(`{"installed_at":"2026-06-02T00:00:00Z"}`), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", path, err)
		}
	}

	RemoveMeta()

	for _, path := range paths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expected %s to be removed, got err=%v", path, err)
		}
	}
}
