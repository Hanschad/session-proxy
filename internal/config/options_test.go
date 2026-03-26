package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOptionsNew(t *testing.T) {
	opt := New()
	if opt.flags == nil {
		t.Fatal("flags should not be nil")
	}
	if opt.viper == nil {
		t.Fatal("viper should not be nil")
	}
}

func TestOptionsParseHelp(t *testing.T) {
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()

	os.Args = []string{"session-proxy", "--help"}
	opt := New()
	if err := opt.Parse(); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !opt.ShowHelp {
		t.Error("ShowHelp should be true")
	}
}

func TestOptionsParseVersion(t *testing.T) {
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()

	os.Args = []string{"session-proxy", "-v"}
	opt := New()
	if err := opt.Parse(); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !opt.ShowVersion {
		t.Error("ShowVersion should be true")
	}
}

func TestOptionsWithConfigFile(t *testing.T) {
	content := `
listen: "127.0.0.1:9999"
upstreams:
  test:
    instances:
      - i-test-1
default: test
`
	tmpFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()

	os.Args = []string{"session-proxy", "-f", tmpFile}
	opt := New()
	if err := opt.Parse(); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	cfg, err := opt.ToConfig()
	if err != nil {
		t.Fatalf("ToConfig: %v", err)
	}

	if cfg.Listen != "127.0.0.1:9999" {
		t.Errorf("expected listen 127.0.0.1:9999, got %s", cfg.Listen)
	}
	if len(cfg.Upstreams) != 1 {
		t.Errorf("expected 1 upstream, got %d", len(cfg.Upstreams))
	}
	if cfg.Default != "test" {
		t.Errorf("expected default test, got %s", cfg.Default)
	}
}

func TestOptionsCLIOverridesFile(t *testing.T) {
	content := `
listen: "127.0.0.1:8888"
upstreams:
  test:
    instances:
      - i-test-1
default: test
`
	tmpFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()

	os.Args = []string{"session-proxy", "-f", tmpFile, "--listen", "0.0.0.0:7777"}
	opt := New()
	if err := opt.Parse(); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	cfg, err := opt.ToConfig()
	if err != nil {
		t.Fatalf("ToConfig: %v", err)
	}

	// CLI should override config file
	if cfg.Listen != "0.0.0.0:7777" {
		t.Errorf("expected listen 0.0.0.0:7777 (CLI override), got %s", cfg.Listen)
	}
}

func TestOptionsLegacyMode(t *testing.T) {
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()

	os.Args = []string{
		"session-proxy",
		"--target", "i-legacy-instance",
		"--region", "eu-west-1",
		"--ssh-user", "ubuntu",
		"--profile", "myprofile",
	}

	opt := New()
	if err := opt.Parse(); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	cfg, err := opt.ToConfig()
	if err != nil {
		t.Fatalf("ToConfig: %v", err)
	}

	if len(cfg.Upstreams) != 1 {
		t.Fatalf("expected 1 upstream, got %d", len(cfg.Upstreams))
	}

	up := cfg.Upstreams["default"]
	if up == nil {
		t.Fatal("default upstream should exist")
	}
	if len(up.Instances) != 1 || up.Instances[0] != "i-legacy-instance" {
		t.Errorf("expected instance i-legacy-instance, got %v", up.Instances)
	}
	if up.SSH.User != "ubuntu" {
		t.Errorf("expected ssh user ubuntu, got %s", up.SSH.User)
	}
	if up.AWS.Region != "eu-west-1" {
		t.Errorf("expected region eu-west-1, got %s", up.AWS.Region)
	}
	if up.AWS.Profile != "myprofile" {
		t.Errorf("expected profile myprofile, got %s", up.AWS.Profile)
	}
}

func TestOptionsEnvOnlySettingLoadsWithoutConfigFile(t *testing.T) {
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	defer func() {
		if chdirErr := os.Chdir(oldWD); chdirErr != nil {
			t.Fatalf("restore wd: %v", chdirErr)
		}
	}()

	tempDir := t.TempDir()
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}

	t.Setenv("SESSION_PROXY_SLEEP_DETECTION_THRESHOLD", "2m")

	os.Args = []string{
		"session-proxy",
		"--target", "i-legacy-instance",
	}

	opt := New()
	if err := opt.Parse(); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	cfg, err := opt.ToConfig()
	if err != nil {
		t.Fatalf("ToConfig: %v", err)
	}

	if cfg.SleepDetectionThreshold != 2*time.Minute {
		t.Fatalf("expected sleep detection threshold 2m, got %v", cfg.SleepDetectionThreshold)
	}
}

func TestOptionsEnvOverridesFileForEnvOnlySetting(t *testing.T) {
	content := `
sleep_detection_threshold: 30s
upstreams:
  test:
    instances:
      - i-test-1
default: test
`
	tmpFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()

	t.Setenv("SESSION_PROXY_SLEEP_DETECTION_THRESHOLD", "2m")

	os.Args = []string{"session-proxy", "-f", tmpFile}
	opt := New()
	if err := opt.Parse(); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	cfg, err := opt.ToConfig()
	if err != nil {
		t.Fatalf("ToConfig: %v", err)
	}

	if cfg.SleepDetectionThreshold != 2*time.Minute {
		t.Fatalf("expected env to override file with 2m, got %v", cfg.SleepDetectionThreshold)
	}
}

func TestOptionsReloadAppliesEnvOnlyOverrides(t *testing.T) {
	content := `
sleep_detection_threshold: 30s
upstreams:
  test:
    instances:
      - i-test-1
default: test
`
	tmpFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()

	os.Args = []string{"session-proxy", "-f", tmpFile}
	opt := New()
	if err := opt.Parse(); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	t.Setenv("SESSION_PROXY_SLEEP_DETECTION_THRESHOLD", "2m")

	cfg, err := opt.reload()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	if cfg.SleepDetectionThreshold != 2*time.Minute {
		t.Fatalf("expected reload env override 2m, got %v", cfg.SleepDetectionThreshold)
	}
}

func TestOptionsReloadUsesWatchedConfigFile(t *testing.T) {
	root := t.TempDir()
	origDir := filepath.Join(root, "orig")
	otherDir := filepath.Join(root, "other")
	if err := os.MkdirAll(origDir, 0o755); err != nil {
		t.Fatalf("mkdir orig: %v", err)
	}
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatalf("mkdir other: %v", err)
	}

	origConfig := filepath.Join(origDir, "config.yaml")
	otherConfig := filepath.Join(otherDir, "config.yaml")

	base := `
sleep_detection_threshold: %s
upstreams:
  test:
    instances:
      - i-test-1
default: test
`

	if err := os.WriteFile(origConfig, []byte(fmt.Sprintf(base, "10s")), 0o644); err != nil {
		t.Fatalf("write orig config: %v", err)
	}
	if err := os.WriteFile(otherConfig, []byte(fmt.Sprintf(base, "20s")), 0o644); err != nil {
		t.Fatalf("write other config: %v", err)
	}

	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	defer func() {
		if chdirErr := os.Chdir(oldWD); chdirErr != nil {
			t.Fatalf("restore wd: %v", chdirErr)
		}
	}()

	if err := os.Chdir(origDir); err != nil {
		t.Fatalf("Chdir orig: %v", err)
	}

	os.Args = []string{"session-proxy"}
	opt := New()
	if err := opt.Parse(); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if opt.ConfigFile == "" {
		t.Fatal("expected Parse to capture discovered config file")
	}

	if err := os.Chdir(otherDir); err != nil {
		t.Fatalf("Chdir other: %v", err)
	}

	cfg, err := opt.reload()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	if cfg.SleepDetectionThreshold != 10*time.Second {
		t.Fatalf("expected reload to keep watched config value 10s, got %v", cfg.SleepDetectionThreshold)
	}
}

func TestOptionsReloadPreservesAuthFlagOverrides(t *testing.T) {
	content := `
upstreams:
  test:
    instances:
      - i-test-1
default: test
`
	tmpFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()

	os.Args = []string{
		"session-proxy",
		"-f", tmpFile,
		"--auth-user", "alice",
		"--auth-pass", "secret",
	}

	opt := New()
	if err := opt.Parse(); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	cfg, err := opt.reload()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	if cfg.Auth == nil {
		t.Fatal("expected auth config after reload")
	}
	if cfg.Auth.User != "alice" || cfg.Auth.Pass != "secret" {
		t.Fatalf("expected auth alice/secret after reload, got %+v", cfg.Auth)
	}
}
