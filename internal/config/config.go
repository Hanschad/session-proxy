package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/viper"
)

// Config is the root configuration structure.
type Config struct {
	Listen    string               `mapstructure:"listen"`
	Auth      *AuthConfig          `mapstructure:"auth"`
	Upstreams map[string]*Upstream `mapstructure:"upstreams"`
	Routes    []Route              `mapstructure:"routes"`
	Default   string               `mapstructure:"default"`
}

// AuthConfig holds optional SOCKS5 authentication (per listen port).
type AuthConfig struct {
	User string `mapstructure:"user"`
	Pass string `mapstructure:"pass"`
}

// Upstream represents a group of EC2 instances in a specific environment.
type Upstream struct {
	SSH       SSHConfig `mapstructure:"ssh"`
	AWS       AWSConfig `mapstructure:"aws"`
	Instances []string  `mapstructure:"instances"`
}

// SSHConfig holds SSH connection settings (per upstream).
type SSHConfig struct {
	User string `mapstructure:"user"`
	Key  string `mapstructure:"key"`
}

// AWSConfig holds AWS credentials configuration.
// If Profile is set, region is auto-detected from AWS config.
// If AccessKey/SecretKey are set, Region must also be specified.
type AWSConfig struct {
	Profile   string `mapstructure:"profile"`
	Region    string `mapstructure:"region"`
	AccessKey string `mapstructure:"access_key"`
	SecretKey string `mapstructure:"secret_key"`
}

// Route defines a routing rule from destination to upstream.
type Route struct {
	Match    string `mapstructure:"match"`
	Upstream string `mapstructure:"upstream"`
}

// Load loads configuration from an explicit path.
func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(path)

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	return unmarshalAndValidate(v)
}

// LoadDefault searches for configuration in standard locations.
func LoadDefault() (*Config, error) {
	v := viper.New()
	v.SetConfigName("config")
	v.SetConfigType("yaml")

	v.AddConfigPath(".")

	xdgConfig := os.Getenv("XDG_CONFIG_HOME")
	if xdgConfig == "" {
		home, _ := os.UserHomeDir()
		xdgConfig = filepath.Join(home, ".config")
	}
	v.AddConfigPath(filepath.Join(xdgConfig, "session-proxy"))

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	return unmarshalAndValidate(v)
}

func unmarshalAndValidate(v *viper.Viper) (*Config, error) {
	cfg := &Config{}
	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (c *Config) validate() error {
	if len(c.Upstreams) == 0 {
		return fmt.Errorf("at least one upstream is required")
	}

	for name, up := range c.Upstreams {
		if len(up.Instances) == 0 {
			return fmt.Errorf("upstream %q has no instances", name)
		}

		// Validate SSH config
		if up.SSH.User == "" {
			up.SSH.User = "root" // Default SSH user
		}

		// Validate AWS config: either profile OR (region + ak/sk)
		hasProfile := up.AWS.Profile != ""
		hasInlineCreds := up.AWS.AccessKey != "" && up.AWS.SecretKey != ""

		if !hasProfile && !hasInlineCreds {
			// Default to "default" profile
			up.AWS.Profile = "default"
		}

		if hasInlineCreds && up.AWS.Region == "" {
			return fmt.Errorf("upstream %q: region is required when using access_key/secret_key", name)
		}
	}

	if c.Default != "" {
		if _, ok := c.Upstreams[c.Default]; !ok {
			return fmt.Errorf("default upstream %q not found", c.Default)
		}
	}

	for i, r := range c.Routes {
		if r.Match == "" {
			return fmt.Errorf("route[%d] has no match pattern", i)
		}
		if _, ok := c.Upstreams[r.Upstream]; !ok {
			return fmt.Errorf("route[%d] references unknown upstream %q", i, r.Upstream)
		}
	}

	if c.Listen == "" {
		c.Listen = "127.0.0.1:28881"
	}

	return nil
}
