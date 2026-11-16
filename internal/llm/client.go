package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

type Config struct {
	BaseURL, APIKey, Model string
	MaxToolRounds          int
	MaxToolResultBytes     int
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
	Usage       Usage     `json:"-"`
}

type Usage struct {
	ToolCalls    int
	ToolRounds   int
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
	CachedInput  int64
	Available    bool
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

type Client struct {
	cfg Config
	api *openai.Client
}

func New(c Config) *Client {
	if c.MaxToolRounds < 1 {
		c.MaxToolRounds = 6
	}
	if c.MaxToolResultBytes < 1 {
		c.MaxToolResultBytes = 64 << 10
	}
	httpClient := &http.Client{Timeout: 4 * time.Minute}
	api := openai.NewClient(
		option.WithAPIKey(c.APIKey),
		option.WithBaseURL(strings.TrimRight(c.BaseURL, "/")),
		option.WithHTTPClient(httpClient),
	)
	return &Client{cfg: c, api: &api}
}

func (c *Client) Review(ctx context.Context, system, user string, tools []Tool, call func(context.Context, string, map[string]any) (any, error)) (Result, error) {
	messages := []openai.ChatCompletionMessageParamUnion{openai.SystemMessage(system), openai.UserMessage(user)}
	jsonFormat := openai.ResponseFormatJSONObjectParam{Type: "json_object"}
	usage := Usage{}
	for round := 0; ; round++ {
		withTools := len(tools) > 0 && round < c.cfg.MaxToolRounds
		params := openai.ChatCompletionNewParams{
			Messages:       messages,
			Model:          openai.ChatModel(c.cfg.Model),
			ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{OfJSONObject: &jsonFormat},
		}
		if withTools {
			for _, tool := range tools {
				if tool.Type != "function" {
					continue
				}
				var schema openai.FunctionParameters
				if err := json.Unmarshal(tool.Function.Parameters, &schema); err != nil {
					return Result{}, fmt.Errorf("decode MCP tool schema: %w", err)
				}
				params.Tools = append(params.Tools, openai.ChatCompletionFunctionTool(openai.FunctionDefinitionParam{
					Name:        tool.Function.Name,
					Description: openai.String(tool.Function.Description),
					Parameters:  schema,
				}))
			}
		}

		completion, err := c.api.Chat.Completions.New(ctx, params)
		if err != nil {
			return Result{Usage: usage}, fmt.Errorf("OpenAI-compatible chat completion: %w", err)
		}
		usage.InputTokens += completion.Usage.PromptTokens
		usage.OutputTokens += completion.Usage.CompletionTokens
		usage.TotalTokens += completion.Usage.TotalTokens
		usage.CachedInput += completion.Usage.PromptTokensDetails.CachedTokens
		usage.Available = usage.Available || completion.Usage.TotalTokens > 0 || completion.Usage.PromptTokens > 0 || completion.Usage.CompletionTokens > 0 || completion.Usage.PromptTokensDetails.CachedTokens > 0
		if len(completion.Choices) == 0 {
			return Result{Usage: usage}, errors.New("AI endpoint returned no choices")
		}
		message := completion.Choices[0].Message
		if len(message.ToolCalls) > 0 && !withTools {
			return Result{Usage: usage}, errors.New("AI exceeded the configured MCP tool rounds")
		}
		if len(message.ToolCalls) == 0 {
			var result Result
			if err := json.Unmarshal([]byte(message.Content), &result); err != nil {
				return Result{Usage: usage}, fmt.Errorf("decode AI review JSON: %w", err)
			}
			if result.Event != "APPROVE" {
				result.Event = "COMMENT"
			}
			result.Usage = usage
			return result, nil
		}

		usage.ToolRounds++
		usage.ToolCalls += len(message.ToolCalls)
		messages = append(messages, message.ToParam())
		for _, toolCall := range message.ToolCalls {
			var args map[string]any
			if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
				return Result{Usage: usage}, fmt.Errorf("decode MCP tool arguments: %w", err)
			}
			value, callErr := call(ctx, toolCall.Function.Name, args)
			if callErr != nil {
				value = map[string]string{"error": callErr.Error()}
			}
			content, err := toolContent(value, c.cfg.MaxToolResultBytes)
			if err != nil {
				return Result{Usage: usage}, err
			}
			messages = append(messages, openai.ToolMessage(content, toolCall.ID))
		}
	}
}

func toolContent(value any, maxBytes int) (string, error) {
	if text, ok := value.(string); ok {
		if len(text) > maxBytes {
			return fmt.Sprintf(`{"error":"MCP tool result exceeded the %d-byte limit; request a narrower result"}`, maxBytes), nil
		}
		return text, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode MCP tool result: %w", err)
	}
	if len(data) > maxBytes {
		return fmt.Sprintf(`{"error":"MCP tool result exceeded the %d-byte limit; request a narrower result"}`, maxBytes), nil
	}
	return string(data), nil
}
