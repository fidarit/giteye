package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	ListenAddr           string                  `yaml:"listen_addr"`
	Gitea                Gitea                   `yaml:"gitea"`
	Webhook              Webhook                 `yaml:"webhook"`
	AI                   AI                      `yaml:"ai"`
	MCP                  MCP                     `yaml:"mcp"`
	NotifyOnFailure      bool                    `yaml:"notify_on_failure"`
	MaxConcurrentReviews int                     `yaml:"max_concurrent_reviews"`
	MaxToolRounds        int                     `yaml:"max_tool_rounds"`
	Limits               Limits                  `yaml:"limits"`
	ReviewLanguage       string                  `yaml:"review_language"`
	SystemPromptOverride string                  `yaml:"system_prompt_override"`
	SystemPromptAddition string                  `yaml:"system_prompt_addition"`
	Repositories         map[string]RepoOverride `yaml:"repositories"`
}

type Limits struct {
	WebhookPayloadBytes      int `yaml:"webhook_payload_bytes"`
	ReviewContextBytes       int `yaml:"review_context_bytes"`
	GiteaReviewCommentsBytes int `yaml:"gitea_review_comments_bytes"`
	MCPToolResultBytes       int `yaml:"mcp_tool_result_bytes"`
	MCPToolListPages         int `yaml:"mcp_tool_list_pages"`
	MCPDiscoveredTools       int `yaml:"mcp_discovered_tools"`
	MCPExposedTools          int `yaml:"mcp_exposed_tools"`
	MCPToolSchemaBytes       int `yaml:"mcp_tool_schema_bytes"`
	MCPTotalSchemaBytes      int `yaml:"mcp_total_schema_bytes"`
}

func DefaultLimits() Limits {
	return Limits{
		WebhookPayloadBytes:      2 << 20,
		ReviewContextBytes:       256 << 10,
		GiteaReviewCommentsBytes: 32 << 20,
		MCPToolResultBytes:       64 << 10,
		MCPToolListPages:         32,
		MCPDiscoveredTools:       256,
		MCPExposedTools:          128,
		MCPToolSchemaBytes:       64 << 10,
		MCPTotalSchemaBytes:      256 << 10,
	}
}

func (l Limits) WithDefaults() Limits {
	d := DefaultLimits()
	l.WebhookPayloadBytes = positiveOr(l.WebhookPayloadBytes, d.WebhookPayloadBytes)
	l.ReviewContextBytes = positiveOr(l.ReviewContextBytes, d.ReviewContextBytes)
	l.GiteaReviewCommentsBytes = positiveOr(l.GiteaReviewCommentsBytes, d.GiteaReviewCommentsBytes)
	l.MCPToolResultBytes = positiveOr(l.MCPToolResultBytes, d.MCPToolResultBytes)
	l.MCPToolListPages = positiveOr(l.MCPToolListPages, d.MCPToolListPages)
	l.MCPDiscoveredTools = positiveOr(l.MCPDiscoveredTools, d.MCPDiscoveredTools)
	l.MCPExposedTools = positiveOr(l.MCPExposedTools, d.MCPExposedTools)
	l.MCPToolSchemaBytes = positiveOr(l.MCPToolSchemaBytes, d.MCPToolSchemaBytes)
	l.MCPTotalSchemaBytes = positiveOr(l.MCPTotalSchemaBytes, d.MCPTotalSchemaBytes)
	return l
}

type Gitea struct {
	URL         string `yaml:"url"`
	Token       string `yaml:"token"`
	BotUsername string `yaml:"bot_username"`
}
type Webhook struct {
	Secret string `yaml:"secret"`
}
type AI struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
	Model   string `yaml:"model"`
}
type MCP struct {
	URL          string   `yaml:"url"`
	Token        string   `yaml:"token"`
	AuthHeader   string   `yaml:"auth_header"`
	AllowedTools []string `yaml:"allowed_tools"`
}
type RepoOverride struct {
	ReviewLanguage       string `yaml:"review_language"`
	SystemPromptOverride string `yaml:"system_prompt_override"`
	SystemPromptAddition string `yaml:"system_prompt_addition"`
	NotifyOnFailure      *bool  `yaml:"notify_on_failure"`
}

type ReviewSettings struct {
	Language             string
	SystemPromptOverride string
	SystemPromptAddition string
	NotifyOnFailure      bool
}

func Defaults() Config {
	return Config{
		ListenAddr:           ":8080",
		NotifyOnFailure:      true,
		MaxConcurrentReviews: 4,
		MaxToolRounds:        6,
		Limits:               DefaultLimits(),
		ReviewLanguage:       "ru",
	}
}

func (c Config) WithDefaults() Config {
	d := Defaults()
	c.ListenAddr = stringOr(c.ListenAddr, d.ListenAddr)
	c.MaxConcurrentReviews = positiveOr(c.MaxConcurrentReviews, d.MaxConcurrentReviews)
	c.MaxToolRounds = positiveOr(c.MaxToolRounds, d.MaxToolRounds)
	c.Limits = c.Limits.WithDefaults()
	c.ReviewLanguage = stringOr(c.ReviewLanguage, d.ReviewLanguage)
	return c
}

func positiveOr(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

func stringOr(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func Load(path string) (Config, error) {
	c := Defaults()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Config{}, fmt.Errorf("read config %q: %w", path, err)
		}
		if err == nil {
			decoder := yaml.NewDecoder(bytes.NewReader(b))
			decoder.KnownFields(true)
			if err := decoder.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
				return Config{}, fmt.Errorf("decode config %q: %w", path, err)
			}
			var extra any
			if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
				if err == nil {
					err = errors.New("multiple YAML documents are not supported")
				}
				return Config{}, fmt.Errorf("decode config %q: %w", path, err)
			}
		}
	}
	applyEnv(&c)
	c = c.WithDefaults()
	c.Gitea.URL = strings.TrimRight(c.Gitea.URL, "/")
	c.AI.BaseURL = strings.TrimRight(c.AI.BaseURL, "/")
	if c.Webhook.Secret == "" {
		return Config{}, errors.New("webhook.secret is required")
	}
	missing := make([]string, 0, 6)
	if c.Gitea.URL == "" {
		missing = append(missing, "gitea.url")
	}
	if c.Gitea.Token == "" {
		missing = append(missing, "gitea.token")
	}
	if c.Gitea.BotUsername == "" {
		missing = append(missing, "gitea.bot_username")
	}
	if c.AI.BaseURL == "" {
		missing = append(missing, "ai.base_url")
	}
	if c.AI.APIKey == "" {
		missing = append(missing, "ai.api_key")
	}
	if c.AI.Model == "" {
		missing = append(missing, "ai.model")
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("required configuration is missing: %s", strings.Join(missing, ", "))
	}
	for _, endpoint := range []struct{ name, value string }{
		{"gitea.url", c.Gitea.URL},
		{"ai.base_url", c.AI.BaseURL},
	} {
		if err := validateHTTPURL(endpoint.name, endpoint.value); err != nil {
			return Config{}, err
		}
	}
	if c.MCP.URL != "" {
		if err := validateHTTPURL("mcp.url", c.MCP.URL); err != nil {
			return Config{}, err
		}
	}
	if c.MCP.AuthHeader != "" && http.CanonicalHeaderKey(c.MCP.AuthHeader) == "" {
		return Config{}, errors.New("mcp.auth_header must be a valid HTTP header name")
	}
	return c, nil
}

func applyEnv(c *Config) {
	values := map[string]*string{
		"GITEYE_LISTEN_ADDR": &c.ListenAddr,
		"GITEA_URL":          &c.Gitea.URL,
		"GITEA_TOKEN":        &c.Gitea.Token,
		"GITEA_BOT_USERNAME": &c.Gitea.BotUsername,
		"WEBHOOK_SECRET":     &c.Webhook.Secret,
		"AI_BASE_URL":        &c.AI.BaseURL,
		"AI_API_KEY":         &c.AI.APIKey,
		"AI_MODEL":           &c.AI.Model,
		"MCP_URL":            &c.MCP.URL,
		"MCP_TOKEN":          &c.MCP.Token,
		"MCP_AUTH_HEADER":    &c.MCP.AuthHeader,
		"REVIEW_LANGUAGE":    &c.ReviewLanguage,
	}
	for name, target := range values {
		if value, ok := os.LookupEnv(name); ok {
			*target = value
		}
	}
	if value, ok := os.LookupEnv("MCP_ALLOWED_TOOLS"); ok {
		c.MCP.AllowedTools = nil
		for _, name := range strings.Split(value, ",") {
			if name = strings.TrimSpace(name); name != "" {
				c.MCP.AllowedTools = append(c.MCP.AllowedTools, name)
			}
		}
	}
}

func validateHTTPURL(name, value string) error {
	parsed, err := url.ParseRequestURI(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil {
		return fmt.Errorf("%s must be an absolute HTTP or HTTPS URL without embedded credentials", name)
	}
	return nil
}

func (c Config) ForRepo(owner, repo string) ReviewSettings {
	settings := ReviewSettings{
		Language:             c.ReviewLanguage,
		SystemPromptOverride: c.SystemPromptOverride,
		SystemPromptAddition: c.SystemPromptAddition,
		NotifyOnFailure:      c.NotifyOnFailure,
	}
	if r, ok := c.Repositories[owner+"/"+repo]; ok {
		if r.ReviewLanguage != "" {
			settings.Language = r.ReviewLanguage
		}
		if r.SystemPromptOverride != "" {
			settings.SystemPromptOverride = r.SystemPromptOverride
		}
		if r.SystemPromptAddition != "" {
			settings.SystemPromptAddition = r.SystemPromptAddition
		}
		if r.NotifyOnFailure != nil {
			settings.NotifyOnFailure = *r.NotifyOnFailure
		}
	}
	return settings
}
