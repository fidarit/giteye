package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Config struct {
	BaseURL, APIKey, Model string
	MaxToolRounds          int
}
type Comment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Body string `json:"body"`
}
type Result struct {
	Event       string    `json:"event"`
	Summary     string    `json:"summary"`
	Comments    []Comment `json:"comments"`
	ThreadReply *string   `json:"thread_reply"`
}
type ChatMessage struct {
	Role       string     `json:"role"`
	Content    any        `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
type Tool struct {
	Type     string   `json:"type"`
	Function Function `json:"function"`
}
type Function struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}
type response struct {
	Choices []struct {
		Message ChatMessage `json:"message"`
	} `json:"choices"`
}

type Client struct {
	cfg  Config
	http *http.Client
}

func New(c Config) *Client {
	if c.MaxToolRounds < 1 {
		c.MaxToolRounds = 6
	}
	return &Client{cfg: c, http: &http.Client{Timeout: 4 * time.Minute}}
}

func (c *Client) Review(ctx context.Context, system, user string, tools []Tool, call func(context.Context, string, map[string]any) (any, error)) (Result, error) {
	messages := []ChatMessage{{Role: "system", Content: system}, {Role: "user", Content: user}}
	for round := 0; ; round++ {
		withTools := len(tools) > 0 && round < c.cfg.MaxToolRounds
		payload := map[string]any{"model": c.cfg.Model, "messages": messages, "response_format": map[string]string{"type": "json_object"}}
		if withTools {
			payload["tools"] = tools
			payload["tool_choice"] = "auto"
		}
		b, _ := json.Marshal(payload)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.cfg.BaseURL, "/")+"/chat/completions", bytes.NewReader(b))
		if err != nil {
			return Result{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
		resp, err := c.http.Do(req)
		if err != nil {
			return Result{}, err
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if readErr != nil {
			return Result{}, readErr
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return Result{}, fmt.Errorf("AI endpoint returned HTTP %d", resp.StatusCode)
		}
		var result response
		if err = json.Unmarshal(data, &result); err != nil {
			return Result{}, err
		}
		if len(result.Choices) == 0 {
			return Result{}, errors.New("AI endpoint returned no choices")
		}
		msg := result.Choices[0].Message
		if len(msg.ToolCalls) > 0 && !withTools {
			return Result{}, errors.New("AI exceeded the configured MCP tool rounds")
		}
		if len(msg.ToolCalls) == 0 {
			var final Result
			raw, ok := msg.Content.(string)
			if !ok {
				return Result{}, errors.New("AI response content is not text")
			}
			if err = json.Unmarshal([]byte(raw), &final); err != nil {
				return Result{}, fmt.Errorf("decode AI review JSON: %w", err)
			}
			if final.Event != "APPROVE" {
				final.Event = "COMMENT"
			}
			if strings.TrimSpace(final.Summary) == "" {
				final.Summary = "Review completed."
			}
			return final, nil
		}
		messages = append(messages, msg)
		for _, tc := range msg.ToolCalls {
			var args map[string]any
			if err = json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				return Result{}, fmt.Errorf("decode MCP tool arguments: %w", err)
			}
			value, callErr := call(ctx, tc.Function.Name, args)
			if callErr != nil {
				value = map[string]string{"error": callErr.Error()}
			}
			content, err := toolContent(value)
			if err != nil {
				return Result{}, err
			}
			messages = append(messages, ChatMessage{Role: "tool", ToolCallID: tc.ID, Content: content})
		}
	}
}

func toolContent(value any) (string, error) {
	if text, ok := value.(string); ok {
		return text, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode MCP tool result: %w", err)
	}
	return string(data), nil
}
