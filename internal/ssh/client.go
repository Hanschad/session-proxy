package ssh

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	sshlib "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/term"
)

type Config struct {
	User           string
	PrivateKeyPath string
}

func Connect(conn net.Conn, cfg Config) (*sshlib.Client, error) {
	authMethods := []sshlib.AuthMethod{}

	// 1. Try SSH Agent
	if socket := os.Getenv("SSH_AUTH_SOCK"); socket != "" {
		agentConn, err := net.DialTimeout("unix", socket, 1*time.Second)
		if err == nil {
			authMethods = append(authMethods, sshlib.PublicKeysCallback(agent.NewClient(agentConn).Signers))
		}
	}

	// 2. Try Private Key
	if cfg.PrivateKeyPath != "" {
		key, err := os.ReadFile(cfg.PrivateKeyPath)
		if err != nil {
			// Don't error out, just skip if reading fails? Or maybe error?
			// Let's print a warning but continue if agent worked?
			// Actually, if user specified a key, they expect it to work.
			return nil, fmt.Errorf("failed to read private key: %w", err)
		}

		signer, err := sshlib.ParsePrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("failed to parse private key: %w", err)
		}

		authMethods = append(authMethods, sshlib.PublicKeys(signer))
	}

	interactiveAuth := false
	if len(authMethods) == 0 {
		interactiveAuth = true
		// Fallback to interactive password
		authMethods = append(authMethods, sshlib.PasswordCallback(promptForPassword))
		authMethods = append(authMethods, sshlib.KeyboardInteractive(func(name, instruction string, questions []string, echos []bool) (answers []string, err error) {
			// Simple fallback for keyboard interactive that ignores name/instruction and just asks for answers (usually password)
			for _, q := range questions {
				fmt.Printf("%s", q)
				pass, err := term.ReadPassword(int(syscall.Stdin))
				fmt.Println()
				if err != nil {
					return nil, err
				}
				answers = append(answers, string(pass))
			}
			return answers, nil
		}))
	}

	// WARNING: InsecureIgnoreHostKey accepts any host key.
	// This is acceptable for SSM tunnels (already authenticated via AWS IAM),
	// but a known_hosts implementation would be more secure.
	clientConfig := &sshlib.ClientConfig{
		User:            cfg.User,
		Auth:            authMethods,
		HostKeyCallback: sshlib.InsecureIgnoreHostKey(),
	}

	// The "address" for NewClientConn is mostly for logging/verification,
	// the actual connection is already established via 'conn'.
	//
	// Important: for SSH-over-SSM, the underlying transport can become half-open and cause
	// NewClientConn to block indefinitely. For non-interactive auth, enforce a hard timeout
	// and close the connection to trigger an upstream reconnect.
	if interactiveAuth {
		c, chans, reqs, err := sshlib.NewClientConn(conn, "ssm-target", clientConfig)
		if err != nil {
			return nil, fmt.Errorf("ssh handshake failed: %w", err)
		}
		return sshlib.NewClient(c, chans, reqs), nil
	}

	const handshakeTimeout = 30 * time.Second
	type result struct {
		c     sshlib.Conn
		chans <-chan sshlib.NewChannel
		reqs  <-chan *sshlib.Request
		err   error
	}

	resCh := make(chan result, 1)
	go func() {
		c, chans, reqs, err := sshlib.NewClientConn(conn, "ssm-target", clientConfig)
		resCh <- result{c: c, chans: chans, reqs: reqs, err: err}
	}()

	t := time.NewTimer(handshakeTimeout)
	defer t.Stop()

	select {
	case res := <-resCh:
		if res.err != nil {
			return nil, fmt.Errorf("ssh handshake failed: %w", res.err)
		}
		return sshlib.NewClient(res.c, res.chans, res.reqs), nil
	case <-t.C:
		_ = conn.Close()
		return nil, fmt.Errorf("ssh handshake timeout after %s", handshakeTimeout)
	}
}

func promptForPassword() (string, error) {
	fmt.Print("SSH Password: ")
	bytePassword, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Println() // Newline after input
	if err != nil {
		return "", err
	}
	return string(bytePassword), nil
}
