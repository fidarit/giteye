package mcpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
	ReadOnly    bool   `json:"-"`
}

type Client struct {
	URL         string
	Token       string
	AuthHeader  string
	LogRequests bool
	Logger      *slog.Logger
}

type Session struct {
	client      *mcp.ClientSession
	logRequests bool
	logger      *slog.Logger
}

func (c Client) Connect(ctx context.Context) (*Session, error) {
	logger := c.Logger
	if logger == nil {
		logger = slog.Default()
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "giteye", Version: "1.0.0"}, nil)
	hc := &http.Client{Timeout: 90 * time.Second}
	if c.Token != "" {
		header := c.AuthHeader
		if header == "" {
			header = "Authorization"
		}
		value := c.Token
		if strings.EqualFold(header, "Authorization") && !strings.Contains(value, " ") {
			value = "Bearer " + value
		}
		hc.Transport = headerTransport{base: http.DefaultTransport, header: header, value: value}
	}
	transport := &mcp.StreamableClientTransport{Endpoint: c.URL, HTTPClient: hc, DisableStandaloneSSE: true}
	started := time.Now()
	session, err := client.Connect(ctx, transport, nil)
	logAPIRequest(logger, c.LogRequests, "initialize", started, "", err, nil)
	if err != nil {
		return nil, err
	}
	return &Session{client: session, logger: logger, logRequests: c.LogRequests}, nil
}

func (s *Session) Close() error {
	if s == nil || s.client == nil {
		return nil
	}
	started := time.Now()
	err := s.client.Close()
	logAPIRequest(s.logger, s.logRequests, "session/close", started, "", err, nil)
	return err
}

type headerTransport struct {
	base          http.RoundTripper
	header, value string
}

func (t headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	q := r.Clone(r.Context())
	q.Header.Set(t.header, t.value)
	return t.base.RoundTrip(q)
}

func List(ctx context.Context, session *Session, maxPages, maxTools int) ([]Tool, error) {
	if session == nil || session.client == nil {
		return nil, errors.New("MCP session is not connected")
	}
	if maxPages < 1 || maxTools < 1 {
		return nil, errors.New("MCP discovery limits must be positive")
	}
	var out []Tool
	cursor := ""
	seenCursors := map[string]struct{}{}
	pages := 0
	for {
		if pages >= maxPages {
			return nil, errors.New("MCP tools list exceeded the page limit")
		}
		pages++
		started := time.Now()
		res, err := session.client.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		attrs := []any{"page", pages}
		if err == nil {
			attrs = append(attrs, "tools_returned", len(res.Tools), "has_more", res.NextCursor != "")
		}
		logAPIRequest(session.logger, session.logRequests, "tools/list", started, "", err, attrs)
		if err != nil {
			return nil, err
		}
		if len(res.Tools) > maxTools-len(out) {
			return nil, errors.New("MCP tools list exceeded the tool limit")
		}
		for _, t := range res.Tools {
			metadata, err := json.Marshal(t)
			if err != nil {
				return nil, err
			}
			var annotations struct {
				Annotations struct {
					ReadOnlyHint bool `json:"readOnlyHint"`
				} `json:"annotations"`
			}
			if err := json.Unmarshal(metadata, &annotations); err != nil {
				return nil, err
			}
			out = append(out, Tool{
				Name: t.Name, Description: t.Description, InputSchema: t.InputSchema,
				ReadOnly: annotations.Annotations.ReadOnlyHint,
			})
		}
		if res.NextCursor == "" {
			break
		}
		if _, seen := seenCursors[res.NextCursor]; seen {
			return nil, errors.New("MCP server repeated a tools-list cursor")
		}
		seenCursors[res.NextCursor] = struct{}{}
		cursor = res.NextCursor
	}
	return out, nil
}
func Call(ctx context.Context, session *Session, name string, args map[string]any) (any, error) {
	if session == nil || session.client == nil {
		return nil, errors.New("MCP session is not connected")
	}
	argsBytes := 0
	if session.logRequests {
		argsBytes = jsonSize(args)
	}
	started := time.Now()
	res, err := session.client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if session.logRequests {
		attrs := []any{"tool", name, "argument_bytes", argsBytes}
		status := ""
		if err == nil {
			result := any(res.Content)
			if res.StructuredContent != nil {
				result = res.StructuredContent
			}
			if res.IsError {
				status = "tool_error"
			}
			attrs = append(attrs, "result_bytes", jsonSize(result))
		}
		logAPIRequest(session.logger, true, "tools/call", started, status, err, attrs)
	}
	if err != nil {
		return nil, err
	}
	if res.StructuredContent != nil {
		return res.StructuredContent, nil
	}
	return res.Content, nil
}

func logAPIRequest(logger *slog.Logger, enabled bool, operation string, started time.Time, status string, err error, attrs []any) {
	if !enabled {
		return
	}
	fields := []any{"provider", "mcp", "operation", operation, "duration", time.Since(started).Round(time.Millisecond)}
	if err != nil {
		status = "failed"
		fields = append(fields, "error_type", fmt.Sprintf("%T", err))
	} else if status == "" {
		status = "completed"
	}
	fields = append(fields, "status", status)
	fields = append(fields, attrs...)
	logger.Info("API request", fields...)
}

func jsonSize(value any) int {
	encoded, err := json.Marshal(value)
	if err != nil {
		return 0
	}
	return len(encoded)
}
