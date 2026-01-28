package config

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// Options handles CLI flags, env vars, and config file loading.
// Priority: CLI flags > env vars > config file > defaults
type Options struct {
	flags *pflag.FlagSet
	viper *viper.Viper

	// Command-line only flags (not in config file)
	ShowVersion bool   `yaml:"-"`
	ShowHelp    bool   `yaml:"-"`
	ShowConfig  bool   `yaml:"-"`
	ConfigFile  string `yaml:"-"`
	Debug       bool   `yaml:"-"`

	// Core config (CLI + file + env)
	Listen  string      `yaml:"listen" mapstructure:"listen"`
	Auth    *AuthConfig `yaml:"auth" mapstructure:"auth"`
	Default string      `yaml:"default" mapstructure:"default"`

	// Upstreams and routes are complex, only from file
	Upstreams map[string]*Upstream `yaml:"upstreams" mapstructure:"upstreams"`
	Routes    []Route              `yaml:"routes" mapstructure:"routes"`

	// Legacy single-target mode (converted to config internally)
	Target     string `yaml:"-"`
	Region     string `yaml:"-"`
	SSHUser    string `yaml:"-"`
	SSHKey     string `yaml:"-"`
	AWSProfile string `yaml:"-"`
}

// New creates Options with all flags defined.
func New() *Options {
	opt := &Options{
		flags: pflag.NewFlagSet(os.Args[0], pflag.ContinueOnError),
		viper: viper.New(),
	}

	// Command-line only
	opt.flags.BoolVarP(&opt.ShowVersion, "version", "v", false, "Print version and exit")
	opt.flags.BoolVarP(&opt.ShowHelp, "help", "h", false, "Print help and exit")
	opt.flags.BoolVarP(&opt.ShowConfig, "print-config", "c", false, "Print effective config and exit")
	opt.flags.StringVarP(&opt.ConfigFile, "config", "f", "", "Path to config file (yaml)")
	opt.flags.BoolVar(&opt.Debug, "debug", false, "Enable debug logging")

	// Core config with defaults
	opt.flags.StringVar(&opt.Listen, "listen", "127.0.0.1:28881", "SOCKS5 listen address")
	opt.flags.StringVar(&opt.Default, "default", "", "Default upstream name")

	// Auth (simplified for CLI)
	opt.flags.String("auth-user", "", "SOCKS5 auth username")
	opt.flags.String("auth-pass", "", "SOCKS5 auth password")

	// Legacy single-target mode flags
	opt.flags.StringVar(&opt.Target, "target", "", "EC2 instance ID (legacy single mode)")
	opt.flags.StringVar(&opt.Region, "region", "us-east-1", "AWS region (legacy mode)")
	opt.flags.StringVar(&opt.SSHUser, "ssh-user", "root", "SSH username (legacy mode)")
	opt.flags.StringVar(&opt.SSHKey, "ssh-key", "", "SSH private key path (legacy mode)")
	opt.flags.StringVar(&opt.AWSProfile, "profile", "", "AWS profile name (legacy mode)")

	// Bind flags to viper
	_ = opt.viper.BindPFlags(opt.flags)

	return opt
}

// Parse parses CLI args, loads config file, and merges all sources.
func (opt *Options) Parse() error {
	if err := opt.flags.Parse(os.Args[1:]); err != nil {
		return err
	}

	if opt.ShowVersion || opt.ShowHelp {
		return nil
	}

	// Setup viper for env vars
	opt.viper.AutomaticEnv()
	opt.viper.SetEnvPrefix("SESSION_PROXY")
	opt.viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))

	// Load config file if specified or search in default locations
	if opt.ConfigFile != "" {
		opt.viper.SetConfigFile(opt.ConfigFile)
		if err := opt.viper.ReadInConfig(); err != nil {
			return fmt.Errorf("read config file %s: %w", opt.ConfigFile, err)
		}
	} else if opt.Target == "" {
		// No config file specified and no legacy target, search default locations
		opt.viper.SetConfigName("config")
		opt.viper.SetConfigType("yaml")
		opt.viper.AddConfigPath(".")

		xdgConfig := os.Getenv("XDG_CONFIG_HOME")
		if xdgConfig == "" {
			if home, err := os.UserHomeDir(); err == nil {
				xdgConfig = filepath.Join(home, ".config")
			}
		}
		if xdgConfig != "" {
			opt.viper.AddConfigPath(filepath.Join(xdgConfig, "session-proxy"))
		}

		// Config file is optional when using legacy mode
		if err := opt.viper.ReadInConfig(); err != nil {
			if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
				return fmt.Errorf("read config: %w", err)
			}
		}
	}

	// Workaround: viper doesn't treat env vars same as config
	for _, key := range opt.viper.AllKeys() {
		val := opt.viper.Get(key)
		opt.viper.Set(key, val)
	}

	// Unmarshal to Options
	if err := opt.viper.Unmarshal(opt); err != nil {
		return fmt.Errorf("unmarshal config: %w", err)
	}

	// Handle auth from CLI flags
	authUser := opt.viper.GetString("auth-user")
	authPass := opt.viper.GetString("auth-pass")
	if authUser != "" && authPass != "" {
		opt.Auth = &AuthConfig{User: authUser, Pass: authPass}
	}

	return nil
}

// ToConfig converts Options to the Config struct used by the application.
func (opt *Options) ToConfig() (*Config, error) {
	// Check if using legacy mode
	if opt.Target != "" && len(opt.Upstreams) == 0 {
		return opt.toLegacyConfig()
	}

	cfg := &Config{
		Listen:    opt.Listen,
		Auth:      opt.Auth,
		Upstreams: opt.Upstreams,
		Routes:    opt.Routes,
		Default:   opt.Default,
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// toLegacyConfig creates a Config from legacy single-target flags.
func (opt *Options) toLegacyConfig() (*Config, error) {
	if opt.Target == "" {
		return nil, fmt.Errorf("--target is required (or use --config for multi-upstream mode)")
	}

	cfg := &Config{
		Listen: opt.Listen,
		Auth:   opt.Auth,
		Upstreams: map[string]*Upstream{
			"default": {
				SSH: SSHConfig{
					User: opt.SSHUser,
					Key:  opt.SSHKey,
				},
				AWS: AWSConfig{
					Profile: opt.AWSProfile,
					Region:  opt.Region,
				},
				Instances: []string{opt.Target},
			},
		},
		Default: "default",
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// PrintUsage prints the usage help.
func (opt *Options) PrintUsage() {
	fmt.Fprintf(os.Stderr, "Usage: %s [options]\n\n", os.Args[0])
	fmt.Fprintln(os.Stderr, "Options:")
	opt.flags.PrintDefaults()
}

// FlagUsages returns the flag usage string.
func (opt *Options) FlagUsages() string {
	return opt.flags.FlagUsages()
}

// Watch watches the config file for changes and calls onChange when it changes.
// Only routes and default can be hot-reloaded; upstream changes require restart.
func (opt *Options) Watch(ctx context.Context, onChange func(*Config, error)) error {
	if opt.ConfigFile == "" {
		// Try to get config file from viper
		opt.ConfigFile = opt.viper.ConfigFileUsed()
	}
	if opt.ConfigFile == "" {
		return fmt.Errorf("no config file to watch")
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create watcher: %w", err)
	}

	go func() {
		defer watcher.Close()

		// Watch the directory to handle editor save patterns
		dir := filepath.Dir(opt.ConfigFile)
		if err := watcher.Add(dir); err != nil {
			log.Printf("[ERROR] Watch dir %s: %v", dir, err)
			return
		}

		configBase := filepath.Base(opt.ConfigFile)

		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				// Only react to writes on our config file
				if filepath.Base(event.Name) != configBase {
					continue
				}
				if event.Op&(fsnotify.Write|fsnotify.Create) == 0 {
					continue
				}

				log.Printf("[INFO] Config file changed, reloading...")
				newCfg, err := opt.reload()
				onChange(newCfg, err)

			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("[ERROR] Watch error: %v", err)
			}
		}
	}()

	log.Printf("[INFO] Watching config file: %s", opt.ConfigFile)
	return nil
}

// reload reloads the config file and returns the new Config.
func (opt *Options) reload() (*Config, error) {
	if err := opt.viper.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("reload config: %w", err)
	}

	// Create a new Options for the reload to avoid mutating current state
	newOpt := &Options{
		flags: opt.flags,
		viper: opt.viper,
	}

	if err := opt.viper.Unmarshal(newOpt); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	// Validate upstreams haven't changed (not hot-reloadable)
	if err := opt.validateUpstreamsUnchanged(newOpt.Upstreams); err != nil {
		return nil, err
	}

	// Preserve CLI-only flags
	newOpt.ConfigFile = opt.ConfigFile
	newOpt.Debug = opt.Debug

	// Keep listen from original (can't hot-reload)
	newOpt.Listen = opt.Listen

	return newOpt.ToConfig()
}

// validateUpstreamsUnchanged checks if upstreams configuration has changed.
func (opt *Options) validateUpstreamsUnchanged(newUpstreams map[string]*Upstream) error {
	if len(opt.Upstreams) != len(newUpstreams) {
		return fmt.Errorf("upstream configuration changed, restart required to apply")
	}
	for name, oldUp := range opt.Upstreams {
		newUp, ok := newUpstreams[name]
		if !ok {
			return fmt.Errorf("upstream configuration changed, restart required to apply")
		}
		if !upstreamsEqual(oldUp, newUp) {
			return fmt.Errorf("upstream configuration changed, restart required to apply")
		}
	}
	return nil
}

// upstreamsEqual compares two Upstream configs for equality.
func upstreamsEqual(a, b *Upstream) bool {
	if a.SSH.User != b.SSH.User || a.SSH.Key != b.SSH.Key {
		return false
	}
	if a.AWS.Profile != b.AWS.Profile || a.AWS.Region != b.AWS.Region ||
		a.AWS.AccessKey != b.AWS.AccessKey || a.AWS.SecretKey != b.AWS.SecretKey {
		return false
	}
	if len(a.Instances) != len(b.Instances) {
		return false
	}
	for i := range a.Instances {
		if a.Instances[i] != b.Instances[i] {
			return false
		}
	}
	return true
}

// IsDebug returns whether debug mode is enabled.
func (opt *Options) IsDebug() bool {
	return opt.Debug
}
