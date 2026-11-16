package mcpbridge

import (
	"context"
	"encoding/json"
	"errors"
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
	URL        string
	Token      string
	AuthHeader string
}

type Session struct {
	client *mcp.ClientSession
}

func (c Client) Connect(ctx context.Context) (*Session, error) {
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
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, err
	}
	return &Session{client: session}, nil
}

func (s *Session) Close() error {
	if s == nil || s.client == nil {
		return nil
	}
	return s.client.Close()
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
		res, err := session.client.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
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
	res, err := session.client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, err
	}
	if res.StructuredContent != nil {
		return res.StructuredContent, nil
	}
	return res.Content, nil
}
