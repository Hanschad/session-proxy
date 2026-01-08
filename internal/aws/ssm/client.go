package ssm

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	mv2 "github.com/aws/aws-sdk-go-v2/service/ssm"
)

type Client struct {
	svc    *mv2.Client
	region string
}

// ClientConfig holds AWS client configuration.
type ClientConfig struct {
	Profile   string // Use AWS config profile (region auto-detected)
	Region    string // Explicit region (required for inline credentials)
	AccessKey string // For inline credentials
	SecretKey string
}

// NewClient creates a new SSM client with the given configuration.
// If Profile is set, region is auto-detected from AWS config.
// If AccessKey/SecretKey are set, Region must be provided.
func NewClient(ctx context.Context, cfg ClientConfig) (*Client, error) {
	var opts []func(*config.LoadOptions) error

	// Profile mode: load profile, region is auto-detected
	if cfg.Profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(cfg.Profile))
	}

	// Explicit region overrides profile region
	if cfg.Region != "" {
		opts = append(opts, config.WithRegion(cfg.Region))
	}

	// Inline credentials mode
	if cfg.AccessKey != "" && cfg.SecretKey != "" {
		opts = append(opts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		))
	}

	awsCfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to load SDK config: %w", err)
	}

	return &Client{
		svc:    mv2.NewFromConfig(awsCfg),
		region: awsCfg.Region,
	}, nil
}

// Region returns the resolved region for this client.
func (c *Client) Region() string {
	return c.region
}

type Session struct {
	TokenValue string
	StreamUrl  string
	SessionId  string
}

func (c *Client) StartSession(ctx context.Context, instanceID string) (*Session, error) {
	input := &mv2.StartSessionInput{
		Target:       aws.String(instanceID),
		DocumentName: aws.String("AWS-StartSSHSession"),
		Parameters: map[string][]string{
			"portNumber": {"22"},
		},
	}

	out, err := c.svc.StartSession(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to start session: %w", err)
	}

	if out.StreamUrl == nil || out.TokenValue == nil {
		return nil, fmt.Errorf("ssm start session response missing stream url or token")
	}

	return &Session{
		StreamUrl:  *out.StreamUrl,
		TokenValue: *out.TokenValue,
		SessionId:  *out.SessionId,
	}, nil
}

// ResumeSession resumes an existing session to get a new token.
func (c *Client) ResumeSession(ctx context.Context, sessionID string) (*Session, error) {
	input := &mv2.ResumeSessionInput{
		SessionId: aws.String(sessionID),
	}

	out, err := c.svc.ResumeSession(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to resume session: %w", err)
	}

	if out.StreamUrl == nil || out.TokenValue == nil {
		return nil, fmt.Errorf("ssm resume session response missing stream url or token")
	}

	return &Session{
		StreamUrl:  *out.StreamUrl,
		TokenValue: *out.TokenValue,
		SessionId:  sessionID,
	}, nil
}
