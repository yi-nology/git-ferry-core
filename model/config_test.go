package model

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configContent := `
server:
  host: 0.0.0.0
  port: 8890
  mode: debug

database:
  driver: sqlite
  dsn: test.db

git:
  backend: gogit
  temp_dir: /tmp/test

sync:
  max_concurrent: 5
  default_timeout: 300
  retry_count: 3
  mirror_lock_fail_closed: true
`
	if err := os.WriteFile(configPath, []byte(configContent), 0o644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.Server.Host != "0.0.0.0" {
		t.Errorf("Expected host 0.0.0.0, got %s", cfg.Server.Host)
	}
	if cfg.Server.Port != 8890 {
		t.Errorf("Expected port 8890, got %d", cfg.Server.Port)
	}
	if cfg.Database.Driver != "sqlite" {
		t.Errorf("Expected driver sqlite, got %s", cfg.Database.Driver)
	}
	if cfg.Database.DSN != "test.db" {
		t.Errorf("Expected dsn test.db, got %s", cfg.Database.DSN)
	}
	if !cfg.Sync.MirrorLockFailClosed {
		t.Error("Expected mirror_lock_fail_closed true, got false")
	}
}

func TestLoadConfig_Validation(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configContent := `
server:
  host: 0.0.0.0
  port: 8890

database:
  driver: sqlite
  dsn: test.db

git:
  temp_dir: /tmp/test
`
	if err := os.WriteFile(configPath, []byte(configContent), 0o644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.Sync.MaxConcurrent != 5 {
		t.Errorf("Expected default MaxConcurrent 5, got %d", cfg.Sync.MaxConcurrent)
	}
	if cfg.Sync.DefaultTimeout != 300 {
		t.Errorf("Expected default DefaultTimeout 300, got %d", cfg.Sync.DefaultTimeout)
	}
	if cfg.Sync.RetryCount != 3 {
		t.Errorf("Expected default RetryCount 3, got %d", cfg.Sync.RetryCount)
	}
}

func TestLoadConfig_MissingDriver(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configContent := `
server:
  host: 0.0.0.0
  port: 8890

database:
  dsn: test.db
`
	if err := os.WriteFile(configPath, []byte(configContent), 0o644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	_, err := LoadConfig(configPath)
	if err == nil {
		t.Fatal("Expected error for missing database driver")
	}
}

func TestLoadConfig_InvalidDriver(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configContent := `
server:
  host: 0.0.0.0
  port: 8890

database:
  driver: postgres
  dsn: test.db
`
	if err := os.WriteFile(configPath, []byte(configContent), 0o644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	_, err := LoadConfig(configPath)
	if err == nil {
		t.Fatal("Expected error for invalid database driver")
	}
}

func TestLoadConfig_MissingDSN(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configContent := `
server:
  host: 0.0.0.0
  port: 8890

database:
  driver: sqlite
`
	if err := os.WriteFile(configPath, []byte(configContent), 0o644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	_, err := LoadConfig(configPath)
	if err == nil {
		t.Fatal("Expected error for missing database dsn")
	}
}

func TestLoadConfig_FileNotFound(t *testing.T) {
	_, err := LoadConfig("/nonexistent/config.yaml")
	if err == nil {
		t.Fatal("Expected error for nonexistent config file")
	}
}

func TestInitDB_SQLite(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := InitDB("sqlite", dbPath)
	if err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}

	if db == nil {
		t.Fatal("expected non-nil db")
	}

	// Verify tables were created
	if !db.Migrator().HasTable(&Platform{}) {
		t.Error("expected Platform table to exist")
	}

	if !db.Migrator().HasTable(&Repo{}) {
		t.Error("expected Repo table to exist")
	}

	if !db.Migrator().HasTable(&SyncTask{}) {
		t.Error("expected SyncTask table to exist")
	}

	if !db.Migrator().HasTable(&SyncRun{}) {
		t.Error("expected SyncRun table to exist")
	}

	if !db.Migrator().HasTable(&SyncRunStep{}) {
		t.Error("expected SyncRunStep table to exist")
	}

	if !db.Migrator().HasTable(&WebhookRule{}) {
		t.Error("expected WebhookRule table to exist")
	}

	if !db.Migrator().HasTable(&WebhookRuleTask{}) {
		t.Error("expected WebhookRuleTask table to exist")
	}

	if !db.Migrator().HasTable(&WebhookEvent{}) {
		t.Error("expected WebhookEvent table to exist")
	}

	if !db.Migrator().HasTable(&OperationLog{}) {
		t.Error("expected OperationLog table to exist")
	}
}

func TestInitDB_UnsupportedDriver(t *testing.T) {
	_, err := InitDB("postgres", "test.db")
	if err == nil {
		t.Fatal("Expected error for unsupported driver")
	}
}

func TestInitDB_Memory(t *testing.T) {
	db, err := InitDB("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}

	if db == nil {
		t.Fatal("expected non-nil db")
	}
}

// TestLoadConfig_CloneCacheDir:sync.clone_cache_dir 从 yaml 载入——
// Clone Cache(P2 同源共享缓存)的开关字段,空=关闭。
func TestLoadConfig_CloneCacheDir(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configContent := `
database:
  driver: sqlite
  dsn: test.db

sync:
  clone_cache_dir: /var/cache/gitferry/clones
`
	if err := os.WriteFile(configPath, []byte(configContent), 0o644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfg.Sync.CloneCacheDir != "/var/cache/gitferry/clones" {
		t.Errorf("Expected clone_cache_dir /var/cache/gitferry/clones, got %q", cfg.Sync.CloneCacheDir)
	}
}

// TestLoadConfig_CloneCacheDirAbsolute:clone_cache_dir 必须是绝对路径——
// 相对路径随进程 cwd 漂移(重启后缓存"凭空消失"重新种),~/ 波浪号不被
// 展开(实测会字面量建出 ~ 目录),两者都 fail-fast 拒绝;空=关闭放行。
func TestLoadConfig_CloneCacheDirAbsolute(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"empty disabled passes", "", false},
		{"absolute passes", "/var/cache/gitferry/clones", false},
		{"relative rejected", "clones/cache", true},
		{"tilde rejected", "~/clones", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{
				Database: DatabaseConfig{Driver: DriverSQLite, DSN: "test.db"},
			}
			cfg.Sync.CloneCacheDir = tc.value
			err := cfg.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for CloneCacheDir %q, got nil", tc.value)
				}
				if !strings.Contains(err.Error(), "clone_cache_dir") {
					t.Errorf("error must name the config key, got: %v", err)
				}
				return
			}
			// 空值与绝对路径必须零错误(基线 database 已填)——守卫不能只拦
			// 「不含 key 的错误」:那会放过「错误地拒绝绝对路径」这一目标回归
			// (真实错误路径总带 key,恒不触发)。(#29)
			if err != nil {
				t.Fatalf("expected no error for CloneCacheDir %q, got: %v", tc.value, err)
			}
		})
	}
}

// TestLoadConfig_CloneCacheMaxMB:容量上限的 yaml/env 标签与 <=0=不限制语义
// ——标签拼错若无此测会静默变成无上限(#31)。
func TestLoadConfig_CloneCacheMaxMB(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	configContent := `
database:
  driver: sqlite
  dsn: test.db

sync:
  clone_cache_max_mb: 512
`
	if err := os.WriteFile(configPath, []byte(configContent), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfg.Sync.CloneCacheMaxMB != 512 {
		t.Errorf("yaml clone_cache_max_mb: got %d, want 512", cfg.Sync.CloneCacheMaxMB)
	}

	// env 覆盖(DefaultOverwrite=true)。
	t.Setenv("GIT_SYNC_CLONE_CACHE_MAX_MB", "256")
	cfg, err = LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig with env failed: %v", err)
	}
	if cfg.Sync.CloneCacheMaxMB != 256 {
		t.Errorf("env overwrite: got %d, want 256", cfg.Sync.CloneCacheMaxMB)
	}

	// <=0 = 不限制造文档语义:Validate 放行。
	neg := &Config{Database: DatabaseConfig{Driver: DriverSQLite, DSN: "test.db"}}
	neg.Sync.CloneCacheMaxMB = -1
	if err := neg.Validate(); err != nil {
		t.Errorf("negative max must pass as unlimited, got: %v", err)
	}
}
