package mcpbridge

import (
	"context"
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
type Client struct{ URL, Token, AuthHeader string }

func (c Client) Connect(ctx context.Context) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "giteye", Version: "1.0.0"}, nil)
	hc := &http.Client{Timeout: 90 * time.Second}
	if c.Token != "" {
		header := c.AuthHeader
		if header == "" {
			header = "Authorization"
		}
		value := c.Token
		if header == "Authorization" && !strings.Contains(value, " ") {
			value = "Bearer " + value
		}
		hc.Transport = headerTransport{base: http.DefaultTransport, header: header, value: value}
	}
	transport := &mcp.StreamableClientTransport{Endpoint: c.URL, HTTPClient: hc, DisableStandaloneSSE: true}
	return client.Connect(ctx, transport, nil)
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

func List(ctx context.Context, session *mcp.ClientSession) ([]Tool, error) {
	var out []Tool
	cursor := ""
	for {
		res, err := session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, t := range res.Tools {
			out = append(out, Tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	return out, nil
}
func Call(ctx context.Context, session *mcp.ClientSession, name string, args map[string]any) (any, error) {
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, err
	}
	if res.StructuredContent != nil {
		return res.StructuredContent, nil
	}
	return res.Content, nil
}
