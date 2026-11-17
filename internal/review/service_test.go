package review

import (
	"net/http/httptest"
	"strings"
	"testing"

	"giteye/internal/config"
	"giteye/internal/gitea"
	"giteye/internal/webhook"
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

func TestReplyLookupRequiresBotCommentAtSameReviewPosition(t *testing.T) {
	comments := []gitea.Comment{
		{ID: 10, Body: "bot finding", User: gitea.User{Login: "giteye"}, ReviewID: 7, Path: "main.go", Position: 14, CreatedAt: "2026-11-17T04:40:00Z"},
		{ID: 11, Body: "user reply", User: gitea.User{Login: "alice"}, ReviewID: 7, Path: "main.go", Position: 14, CreatedAt: "2026-11-17T04:48:23Z"},
	}
	root, reply, ok := resolveThreadReply(webhook.Comment{ID: 11}, "alice", comments, "giteye")
	if !ok || root.ID != 10 || reply.ID != 11 {
		t.Fatalf("expected root/reply 10/11, got %#v/%#v, %v", root, reply, ok)
	}
	if context := threadContext(root, reply, comments); !strings.Contains(context, "current reply") {
		t.Fatalf("thread context did not include the current reply: %s", context)
	}
	comments[0].User = gitea.User{Login: "mallory"}
	if _, _, ok := resolveThreadReply(webhook.Comment{ID: 11}, "alice", comments, "giteye"); ok {
		t.Fatal("reply to non-bot comment was treated as an active bot thread")
	}
	comments[0].User = gitea.User{Login: "giteye"}
	comments = append(comments, gitea.Comment{ID: 12, Body: "another bot finding", User: gitea.User{Login: "giteye"}, ReviewID: 7, Path: "main.go", Position: 14, CreatedAt: "2026-11-17T04:41:00Z"})
	if _, _, ok := resolveThreadReply(webhook.Comment{ID: 11}, "alice", comments, "giteye"); ok {
		t.Fatal("ambiguous bot comments at the same position were treated as one thread")
	}
}
