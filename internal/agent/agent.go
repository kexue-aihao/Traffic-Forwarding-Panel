package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"trafficpanel/internal/domain"
	"trafficpanel/internal/security"
)

type Client struct {
	serverURL  string
	nodeID     int64
	secret     string
	httpClient *http.Client
}

func New(serverURL string, nodeID int64, secret string) *Client {
	return &Client{
		serverURL:  strings.TrimRight(serverURL, "/"),
		nodeID:     nodeID,
		secret:     secret,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *Client) Sign(payload []byte) string {
	return security.SignMessage(c.secret, payload)
}

func (c *Client) Encrypt(payload []byte) (string, error) {
	return security.Encrypt(security.DeriveKey(c.secret), payload)
}

func (c *Client) Decrypt(encoded string) ([]byte, error) {
	return security.Decrypt(security.DeriveKey(c.secret), encoded)
}

func (c *Client) Register(ctx context.Context, node domain.Node) error {
	data, err := json.Marshal(node)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.serverURL+"/api/nodes/register", strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Node-ID", fmt.Sprintf("%d", c.nodeID))
	req.Header.Set("X-Node-Sign", c.Sign(data))
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("register failed: %s", resp.Status)
	}
	return nil
}

func (c *Client) ReportUsage(ctx context.Context, report domain.UsageReport) error {
	data, err := json.Marshal(report)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.serverURL+"/api/nodes/report", strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Node-ID", fmt.Sprintf("%d", c.nodeID))
	req.Header.Set("X-Node-Sign", c.Sign(data))
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("report failed: %s", resp.Status)
	}
	return nil
}

func (c *Client) FetchCommands(ctx context.Context) ([]domain.NodeCommand, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.serverURL+"/api/nodes/commands?node_id="+url.QueryEscape(fmt.Sprintf("%d", c.nodeID)), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Node-ID", fmt.Sprintf("%d", c.nodeID))
	req.Header.Set("X-Node-Sign", c.Sign([]byte(fmt.Sprintf("%d", c.nodeID))))
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch commands failed: %s", resp.Status)
	}
	var commands []domain.NodeCommand
	if err := json.NewDecoder(resp.Body).Decode(&commands); err != nil {
		return nil, err
	}
	return commands, nil
}

func (c *Client) AcknowledgeCommands(ctx context.Context, ids []int64) error {
	payload, err := json.Marshal(map[string]any{"ids": ids})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.serverURL+"/api/nodes/commands/ack", strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Node-ID", fmt.Sprintf("%d", c.nodeID))
	req.Header.Set("X-Node-Sign", c.Sign(payload))
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("ack failed: %s", resp.Status)
	}
	return nil
}

func (c *Client) ValidateSecret(secret string) error {
	if strings.TrimSpace(secret) == "" {
		return errors.New("empty secret")
	}
	return nil
}
