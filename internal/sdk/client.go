// Package sdk helps Go modules talk to the hub: registration, action calls,
// job lifecycle and events. It is self-contained so it can be extracted to
// its own repo without importing hub internals.
package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Error mirrors the hub's error envelope.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// Client is a hub API client from a module's perspective.
type Client struct {
	HubURL string
	Name   string
	Token  string
	http   *http.Client
}

// NewClient creates a client. name/token identify this module to the hub.
func NewClient(hubURL, name, token string) *Client {
	return &Client{
		HubURL: hubURL,
		Name:   name,
		Token:  token,
		http:   &http.Client{Timeout: 60 * time.Second},
	}
}

// Register dynamically registers this module with the hub. The module's
// token (c.Token) is passed as a pre-shared token so it can authenticate
// immediately.
func (c *Client) Register(ctx context.Context, baseURL, version string, capabilities []string) error {
	body := map[string]any{
		"name":         c.Name,
		"base_url":     baseURL,
		"version":      version,
		"capabilities": capabilities,
		"token":        c.Token,
	}
	_, err := c.do(ctx, http.MethodPost, "/v1/modules/register", body)
	return err
}

// Call invokes a synchronous action on another module via the hub.
func (c *Client) Call(ctx context.Context, module, action string, params any, out any) error {
	body := map[string]any{"params": params}
	res, err := c.do(ctx, http.MethodPost, "/v1/call/"+module+"/"+action, body)
	if err != nil {
		return err
	}
	return unmarshalResult(res, out)
}

// CreateJob starts a job on a module via the hub.
func (c *Client) CreateJob(ctx context.Context, module, action string, params any) (string, error) {
	body := map[string]any{"module": module, "action": action, "params": params}
	res, err := c.do(ctx, http.MethodPost, "/v1/jobs", body)
	if err != nil {
		return "", err
	}
	var out struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", err
	}
	return out.JobID, nil
}

// ReportProgress sends a progress update for one of this module's jobs.
func (c *Client) ReportProgress(ctx context.Context, jobID string, p float64, stage, msg string) error {
	body := map[string]any{"progress": p, "stage": stage, "message": msg}
	_, err := c.do(ctx, http.MethodPost, "/v1/callbacks/jobs/"+jobID+"/progress", body)
	return err
}

// Complete reports a job's successful result.
func (c *Client) Complete(ctx context.Context, jobID string, result any) error {
	body := map[string]any{"result": result}
	_, err := c.do(ctx, http.MethodPost, "/v1/callbacks/jobs/"+jobID+"/complete", body)
	return err
}

// Fail reports a job failure.
func (c *Client) Fail(ctx context.Context, jobID, code, message string) error {
	body := map[string]any{"code": code, "message": message}
	_, err := c.do(ctx, http.MethodPost, "/v1/callbacks/jobs/"+jobID+"/fail", body)
	return err
}

// EmitEvent publishes a custom event through the hub's event bus.
func (c *Client) EmitEvent(ctx context.Context, typ string, payload any) error {
	body := map[string]any{"type": typ, "payload": payload}
	_, err := c.do(ctx, http.MethodPost, "/v1/callbacks/events", body)
	return err
}

func (c *Client) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.HubURL+path, payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var env struct {
			Error *Error `json:"error"`
		}
		if json.Unmarshal(data, &env) == nil && env.Error != nil {
			return nil, env.Error
		}
		return nil, fmt.Errorf("hub: status %d: %s", resp.StatusCode, truncate(string(data)))
	}
	return data, nil
}

func unmarshalResult(data []byte, out any) error {
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Result, out)
}

func truncate(s string) string {
	const max = 200
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
