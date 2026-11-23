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

func TestValidateReviewOutput(t *testing.T) {
	tests := []struct {
		name     string
		event    string
		summary  string
		comments []gitea.InlineComment
		wantErr  bool
	}{
		{name: "empty approval is valid", event: "APPROVE"},
		{name: "empty comment is invalid", event: "COMMENT", wantErr: true},
		{name: "summary is valid", event: "COMMENT", summary: "Looks good"},
		{name: "inline finding is valid", event: "COMMENT", comments: []gitea.InlineComment{{Path: "main.go", Line: 1, Body: "Bug"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateReviewOutput(test.event, test.summary, test.comments)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateReviewOutput() error = %v, wantErr %v", err, test.wantErr)
			}
		})
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
	comments = append(comments,
		gitea.Comment{ID: 12, Body: "first bot reply", User: gitea.User{Login: "giteye"}, ReviewID: 7, Path: "main.go", Position: 14, CreatedAt: "2026-11-17T04:49:00Z"},
		gitea.Comment{ID: 13, Body: "later user reply", User: gitea.User{Login: "alice"}, ReviewID: 7, Path: "main.go", Position: 14, CreatedAt: "2026-11-17T04:50:00Z"},
	)
	root, reply, ok = resolveThreadReply(webhook.Comment{ID: 13}, "alice", comments, "giteye")
	if !ok || root.ID != 10 || reply.ID != 13 {
		t.Fatalf("expected original bot finding/current user reply 10/13, got %#v/%#v, %v", root, reply, ok)
	}
	context := threadContext(root, reply, comments)
	for _, expected := range []string{
		"Root comment #10 by Giteye: bot finding",
		"Reply #11 by alice: user reply",
		"Reply #12 by giteye: first bot reply",
		"Reply #13 by alice (current reply): later user reply",
	} {
		if !strings.Contains(context, expected) {
			t.Errorf("thread context missing %q:\n%s", expected, context)
		}
	}
	if strings.Index(context, "#11") > strings.Index(context, "#12") || strings.Index(context, "#12") > strings.Index(context, "#13") {
		t.Errorf("thread replies are not chronological:\n%s", context)
	}
}
