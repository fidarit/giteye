package mcpbridge

import (
	"context"
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

func List(ctx context.Context, session *Session) ([]Tool, error) {
	if session == nil || session.client == nil {
		return nil, errors.New("MCP session is not connected")
	}
	var out []Tool
	cursor := ""
	seenCursors := map[string]struct{}{}
	for {
		res, err := session.client.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, t := range res.Tools {
			out = append(out, Tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
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
