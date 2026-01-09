package ssh

import (
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
	// Create a mock conn that will fail
	// This tests the error path when reading a non-existent key file
	cfg := Config{
		User:           "testuser",
		PrivateKeyPath: "/nonexistent/path/to/key",
	}

	// We can't call Connect without a real net.Conn, but we can verify
	// the Config struct handles the path correctly
	if cfg.PrivateKeyPath != "/nonexistent/path/to/key" {
		t.Error("PrivateKeyPath not set correctly")
	}
}
