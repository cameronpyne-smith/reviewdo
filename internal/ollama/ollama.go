package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	url         string
	model       string
	numCtx      int
	temperature float64
	think       *bool
	http        *http.Client
}

func New(url, model string, numCtx int, temperature float64, think *bool, timeout time.Duration) *Client {
	return &Client{
		url:         url,
		model:       model,
		numCtx:      numCtx,
		temperature: temperature,
		think:       think,
		http:        &http.Client{Timeout: timeout},
	}
}

func (c *Client) Model() string { return c.model }

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model     string          `json:"model"`
	Messages  []message       `json:"messages"`
	Stream    bool            `json:"stream"`
	Format    json.RawMessage `json:"format,omitempty"`
	Think     *bool           `json:"think,omitempty"`
	KeepAlive string          `json:"keep_alive"`
	Options   map[string]any  `json:"options"`
}

type chatResponse struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	Done            bool   `json:"done"`
	Error           string `json:"error"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
	TotalDuration   int64  `json:"total_duration"`
}

type Usage struct {
	PromptTokens int
	OutputTokens int
	Duration     time.Duration
}

func (c *Client) Chat(ctx context.Context, system, user string, schema json.RawMessage) (string, Usage, error) {
	req := chatRequest{
		Model: c.model,
		Messages: []message{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Format:    schema,
		Think:     c.think,
		KeepAlive: "15m",
		Options: map[string]any{
			"num_ctx":     c.numCtx,
			"temperature": c.temperature,
		},
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", Usage{}, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(hr)
	if err != nil {
		return "", Usage{}, fmt.Errorf("ollama: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
	if err != nil {
		return "", Usage{}, err
	}
	var out chatResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return "", Usage{}, fmt.Errorf("ollama: %d %s", resp.StatusCode, truncate(string(data), 300))
	}
	if out.Error != "" {
		return "", Usage{}, fmt.Errorf("ollama: %s", out.Error)
	}
	if resp.StatusCode >= 300 {
		return "", Usage{}, fmt.Errorf("ollama: status %d", resp.StatusCode)
	}
	u := Usage{
		PromptTokens: out.PromptEvalCount,
		OutputTokens: out.EvalCount,
		Duration:     time.Duration(out.TotalDuration),
	}
	return out.Message.Content, u, nil
}

func (c *Client) Models(ctx context.Context) ([]string, error) {
	hr, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("ollama: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("ollama: %w", err)
	}
	names := make([]string, 0, len(out.Models))
	for _, m := range out.Models {
		names = append(names, m.Name)
	}
	return names, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
