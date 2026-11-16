package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnvironmentAndRepoOverrides(t *testing.T) {
	for key, value := range map[string]string{
		"GITEA_URL": "https://gitea.example.test", "GITEA_TOKEN": "gitea-token", "GITEA_BOT_USERNAME": "giteye",
		"WEBHOOK_SECRET": "webhook-secret", "AI_BASE_URL": "https://ai.example.test/v1", "AI_API_KEY": "ai-key", "AI_MODEL": "model",
	} {
		t.Setenv(key, value)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := []byte("review_language: ru\nrepositories:\n  org/repo:\n    review_language: en\n    notify_on_failure: false\n")
	if err := os.WriteFile(path, yaml, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Gitea.URL != "https://gitea.example.test" || c.Gitea.Token != "gitea-token" || c.AI.Model != "model" {
		t.Fatalf("environment values were not applied: %#v", c)
	}
	if !c.NotifyOnFailure {
		t.Fatal("global failure notification should default to true")
	}
	settings := c.ForRepo("org", "repo")
	if settings.Language != "en" || settings.NotifyOnFailure {
		t.Fatalf("repository override not applied: language=%q notify=%v", settings.Language, settings.NotifyOnFailure)
	}
	if c.MaxToolRounds != 6 || c.MaxConcurrentReviews != 4 {
		t.Fatalf("unexpected defaults: rounds=%d concurrency=%d", c.MaxToolRounds, c.MaxConcurrentReviews)
	}
}
