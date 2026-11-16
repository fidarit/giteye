package review

import (
	"net/http/httptest"
	"testing"

	"giteye/internal/config"
	"giteye/internal/gitea"
)

func TestHealthz(t *testing.T) {
	service, err := NewService(config.Config{
		Gitea: config.Gitea{URL: "https://gitea.example.test", Token: "token"},
		AI:    config.AI{BaseURL: "https://ai.example.test/v1", APIKey: "key", Model: "test-model"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/healthz", nil))
	if response.Code != 200 {
		t.Fatalf("health endpoint returned %d", response.Code)
	}
}

func TestReplyLookupRequiresBotAncestor(t *testing.T) {
	comments := []gitea.Comment{
		{ID: 10, Body: "bot finding", User: gitea.User{Login: "giteye"}},
		{ID: 11, Body: "user reply", User: gitea.User{Login: "alice"}, InReplyTo: 10, ReviewID: 7},
	}
	if comment, ok := reviewReply(7, "alice", comments); !ok || comment.ID != 11 {
		t.Fatalf("expected reply comment 11, got %#v, %v", comment, ok)
	}
	if _, ok := threadRoot(11, comments, "giteye"); !ok {
		t.Fatal("reply to bot comment was not recognized")
	}
	comments[0].User = gitea.User{Login: "mallory"}
	if _, ok := threadRoot(11, comments, "giteye"); ok {
		t.Fatal("reply to non-bot comment was treated as an active bot thread")
	}
}
