package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadYAML(t *testing.T) {
	content := `
listen: "127.0.0.1:8080"
upstreams:
  dev:
    ssh:
      user: testuser
      key: ~/.ssh/test_key
    aws:
      profile: default
    instances:
      - i-dev-1
      - i-dev-2
  uat:
    ssh:
      user: admin
    aws:
      profile: prod
    instances:
      - i-uat-1
routes:
  - match: "10.0.0.0/8"
    upstream: dev
  - match: "*.dev.internal"
    upstream: dev
default: dev
`
	tmpFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Listen != "127.0.0.1:8080" {
		t.Errorf("expected listen 127.0.0.1:8080, got %s", cfg.Listen)
	}
	if cfg.Upstreams["dev"].SSH.User != "testuser" {
		t.Errorf("expected dev ssh user testuser, got %s", cfg.Upstreams["dev"].SSH.User)
	}
	if len(cfg.Upstreams) != 2 {
		t.Errorf("expected 2 upstreams, got %d", len(cfg.Upstreams))
	}
	if cfg.Upstreams["dev"].AWS.Profile != "default" {
		t.Errorf("expected dev profile default, got %s", cfg.Upstreams["dev"].AWS.Profile)
	}
	if len(cfg.Routes) != 2 {
		t.Errorf("expected 2 routes, got %d", len(cfg.Routes))
	}
	if cfg.Default != "dev" {
		t.Errorf("expected default dev, got %s", cfg.Default)
	}
}

func TestLoadTOML(t *testing.T) {
	content := `
listen = "127.0.0.1:9090"

[upstreams.prod]
instances = ["i-prod-1"]

[upstreams.prod.ssh]
user = "admin"

[upstreams.prod.aws]
profile = "production"
region = "eu-west-1"

[[routes]]
match = "192.168.0.0/16"
upstream = "prod"

default = "prod"
`
	tmpFile := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Listen != "127.0.0.1:9090" {
		t.Errorf("expected listen 127.0.0.1:9090, got %s", cfg.Listen)
	}
	if cfg.Upstreams["prod"].SSH.User != "admin" {
		t.Errorf("expected ssh user admin, got %s", cfg.Upstreams["prod"].SSH.User)
	}
	if cfg.Upstreams["prod"].AWS.Region != "eu-west-1" {
		t.Errorf("expected region eu-west-1, got %s", cfg.Upstreams["prod"].AWS.Region)
	}
}

func TestValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name:    "no upstreams",
			content: `listen: ":8080"`,
			wantErr: "at least one upstream is required",
		},
		{
			name: "no instances",
			content: `
upstreams:
  dev:
    instances: []
`,
			wantErr: `upstream "dev" has no instances`,
		},
		{
			name: "unknown default",
			content: `
upstreams:
  dev:
    instances: [i-1]
default: unknown
`,
			wantErr: `default upstream "unknown" not found`,
		},
		{
			name: "route unknown upstream",
			content: `
upstreams:
  dev:
    instances: [i-1]
routes:
  - match: "10.0.0.0/8"
    upstream: nonexistent
`,
			wantErr: `references unknown upstream "nonexistent"`,
		},
		{
			name: "inline creds without region",
			content: `
upstreams:
  dev:
    aws:
      access_key: AKIA123
      secret_key: secret
    instances: [i-1]
`,
			wantErr: `region is required when using access_key/secret_key`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpFile := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(tmpFile, []byte(tt.content), 0644); err != nil {
				t.Fatalf("write temp file: %v", err)
			}

			_, err := Load(tmpFile)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !containsSubstr(err.Error(), tt.wantErr) {
				t.Errorf("expected error containing %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}

func TestDefaults(t *testing.T) {
	content := `
upstreams:
  dev:
    instances: [i-1]
`
	tmpFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Listen != "127.0.0.1:28881" {
		t.Errorf("expected default listen 127.0.0.1:28881, got %s", cfg.Listen)
	}
	if cfg.Upstreams["dev"].SSH.User != "root" {
		t.Errorf("expected default ssh user root, got %s", cfg.Upstreams["dev"].SSH.User)
	}
	if cfg.Upstreams["dev"].AWS.Profile != "default" {
		t.Errorf("expected default aws profile, got %s", cfg.Upstreams["dev"].AWS.Profile)
	}
}

func containsSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
