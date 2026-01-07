package ssm

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	mv2 "github.com/aws/aws-sdk-go-v2/service/ssm"
)

type Client struct {
	svc *mv2.Client
}

func NewClient(ctx context.Context, region string) (*Client, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("unable to load SDK config: %w", err)
	}

	return &Client{
		svc: mv2.NewFromConfig(cfg),
	}, nil
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
// This is used for reconnection after a network interruption.
// The session must still be valid on the server side.
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
