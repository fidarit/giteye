package config

import (
	"errors"
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
	ReviewLanguage       string                  `yaml:"review_language"`
	SystemPromptOverride string                  `yaml:"system_prompt_override"`
	SystemPromptAddition string                  `yaml:"system_prompt_addition"`
	Repositories         map[string]RepoOverride `yaml:"repositories"`
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
	URL        string `yaml:"url"`
	Token      string `yaml:"token"`
	AuthHeader string `yaml:"auth_header"`
}
type RepoOverride struct {
	ReviewLanguage       string `yaml:"review_language"`
	SystemPromptOverride string `yaml:"system_prompt_override"`
	SystemPromptAddition string `yaml:"system_prompt_addition"`
	NotifyOnFailure      *bool  `yaml:"notify_on_failure"`
}

func Load(path string) (Config, error) {
	c := Config{ListenAddr: ":8080", NotifyOnFailure: true, MaxConcurrentReviews: 4, MaxToolRounds: 6, ReviewLanguage: "ru"}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return c, err
		}
		if err == nil {
			if err := yaml.Unmarshal(b, &c); err != nil {
				return c, err
			}
		}
	}
	set := func(key string, dst *string) {
		if v, ok := os.LookupEnv(key); ok {
			*dst = v
		}
	}
	set("GITEYE_LISTEN_ADDR", &c.ListenAddr)
	set("GITEA_URL", &c.Gitea.URL)
	set("GITEA_TOKEN", &c.Gitea.Token)
	set("GITEA_BOT_USERNAME", &c.Gitea.BotUsername)
	set("WEBHOOK_SECRET", &c.Webhook.Secret)
	set("AI_BASE_URL", &c.AI.BaseURL)
	set("AI_API_KEY", &c.AI.APIKey)
	set("AI_MODEL", &c.AI.Model)
	set("MCP_URL", &c.MCP.URL)
	set("MCP_TOKEN", &c.MCP.Token)
	set("MCP_AUTH_HEADER", &c.MCP.AuthHeader)
	set("REVIEW_LANGUAGE", &c.ReviewLanguage)
	if c.ListenAddr == "" {
		c.ListenAddr = ":8080"
	}
	if c.MaxConcurrentReviews <= 0 {
		c.MaxConcurrentReviews = 4
	}
	if c.MaxToolRounds <= 0 {
		c.MaxToolRounds = 6
	}
	if c.ReviewLanguage == "" {
		c.ReviewLanguage = "ru"
	}
	c.Gitea.URL = strings.TrimRight(c.Gitea.URL, "/")
	if c.Webhook.Secret == "" {
		return c, errors.New("webhook.secret is required")
	}
	if c.Gitea.URL == "" || c.Gitea.Token == "" || c.Gitea.BotUsername == "" || c.AI.BaseURL == "" || c.AI.APIKey == "" || c.AI.Model == "" {
		return c, errors.New("gitea url/token/bot_username and ai base_url/api_key/model are required")
	}
	return c, nil
}

func (c Config) ForRepo(owner, repo string) (string, string, string, bool) {
	language, override, addition, notify := c.ReviewLanguage, c.SystemPromptOverride, c.SystemPromptAddition, c.NotifyOnFailure
	if r, ok := c.Repositories[owner+"/"+repo]; ok {
		if r.ReviewLanguage != "" {
			language = r.ReviewLanguage
		}
		if r.SystemPromptOverride != "" {
			override = r.SystemPromptOverride
		}
		if r.SystemPromptAddition != "" {
			addition = r.SystemPromptAddition
		}
		if r.NotifyOnFailure != nil {
			notify = *r.NotifyOnFailure
		}
	}
	return language, override, addition, notify
}
