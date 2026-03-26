package ssh

import (
	"net"
	"testing"
)

func TestConfigDefaults(t *testing.T) {
	cfg := Config{}
	if cfg.User != "" {
		t.Errorf("Default User should be empty, got %q", cfg.User)
	}
	if cfg.PrivateKeyPath != "" {
		t.Errorf("Default PrivateKeyPath should be empty, got %q", cfg.PrivateKeyPath)
	}
}

func TestConfigWithValues(t *testing.T) {
	cfg := Config{
		User:           "admin",
		PrivateKeyPath: "/home/user/.ssh/id_rsa",
	}

	if cfg.User != "admin" {
		t.Errorf("User = %q, want admin", cfg.User)
	}
	if cfg.PrivateKeyPath != "/home/user/.ssh/id_rsa" {
		t.Errorf("PrivateKeyPath = %q, want /home/user/.ssh/id_rsa", cfg.PrivateKeyPath)
	}
}

// Note: Testing Connect() requires either:
// 1. A real SSH server (integration test)
// 2. Mocking the net.Conn and SSH handshake (complex)
//
// The current tests cover config structure. Full integration tests
// should be added when an SSH test server is available.

func TestConnectWithInvalidKey(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")

	cfg := Config{
		User:           "testuser",
		PrivateKeyPath: "/nonexistent/path/to/key",
	}

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	_, err := Connect(client, cfg)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "failed to read private key: open /nonexistent/path/to/key: no such file or directory" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConnectFailsFastWithoutNonInteractiveAuth(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	_, err := Connect(client, Config{User: "testuser"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "no non-interactive SSH authentication available: configure SSH_AUTH_SOCK or ssh.key" {
		t.Fatalf("unexpected error: %v", err)
	}
}
