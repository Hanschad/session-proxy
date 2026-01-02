package ssh

import (
	"fmt"
	"net"
	"os"
	"syscall"

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
		agentConn, err := net.Dial("unix", socket)
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

	if len(authMethods) == 0 {
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

	clientConfig := &sshlib.ClientConfig{
		User:            cfg.User,
		Auth:            authMethods,
		HostKeyCallback: sshlib.InsecureIgnoreHostKey(), // TODO: Make this secure via known_hosts
		// HostKeyCallback: sshlib.FixedHostKey(pk), // Ideally we read multiple keys
	}

	// The "address" for NewClientConn is mostly for logging/verification,
	// the actual connection is already established via 'conn'.
	c, chans, reqs, err := sshlib.NewClientConn(conn, "ssm-target", clientConfig)
	if err != nil {
		return nil, fmt.Errorf("ssh handshake failed: %w", err)
	}

	return sshlib.NewClient(c, chans, reqs), nil
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
