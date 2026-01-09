package config

import (
	"os"
	"path/filepath"
	"testing"
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
