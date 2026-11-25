package review

import (
	"net/http/httptest"
	"strings"
	"testing"

	"giteye/internal/config"
	"giteye/internal/diffvalidate"
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

func TestReviewContextPutsHistoryBeforeDiff(t *testing.T) {
	d := contextData{
		Diff:    "diff-marker",
		Reviews: []gitea.Review{{Body: "history-marker"}},
	}
	context := reviewContext("owner", "repo", d, "bot", true)
	if strings.Index(context, "history-marker") > strings.Index(context, "diff-marker") {
		t.Fatal("review context puts the diff before review history")
	}
}

func TestSplitDiffPreservesChangedLineAnchors(t *testing.T) {
	diff := "diff --git a/main.go b/main.go\nindex 000..111 100644\n--- a/main.go\n+++ b/main.go\n" +
		"@@ -1,2 +1,3 @@\n package main\n+func first() {}\n \n" +
		"@@ -10,2 +11,3 @@ function\n func existing() {}\n+func second() {}\n end\n"
	chunks, err := splitDiff(diff, 145)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 {
		t.Fatalf("splitDiff() returned %d chunks, want multiple", len(chunks))
	}
	want := diffvalidate.DiffLines(diff)
	got := make(map[string]map[int]struct{})
	for _, chunk := range chunks {
		if len(chunk) > 145 {
			t.Errorf("chunk has %d bytes, exceeds limit", len(chunk))
		}
		for path, lines := range diffvalidate.DiffLines(chunk) {
			if got[path] == nil {
				got[path] = make(map[int]struct{})
			}
			for line := range lines {
				got[path][line] = struct{}{}
			}
		}
	}
	for path, lines := range want {
		for line := range lines {
			if _, ok := got[path][line]; !ok {
				t.Errorf("split chunks lost changed line %s:%d", path, line)
			}
		}
	}
}

func TestSplitDiffRejectsImpossibleBudgets(t *testing.T) {
	if _, err := splitDiff("", 100); err == nil {
		t.Fatal("splitDiff() accepted an empty diff")
	}
	diff := "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-old\n+new\n"
	if _, err := splitDiff(diff, 8); err == nil {
		t.Fatal("splitDiff() accepted a hunk larger than the available context")
	}
}

func TestSplitDiffPreservesZeroLengthHunkStarts(t *testing.T) {
	tests := []struct {
		name string
		hunk string
		line string
		want string
	}{
		{name: "insertion", hunk: "@@ -3,0 +4,1 @@", line: "+added\n", want: "@@ -3,0 +4,1 @@"},
		{name: "deletion", hunk: "@@ -4,1 +3,0 @@", line: "-removed\n", want: "@@ -4,1 +3,0 @@"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			diff := "diff --git a/file.txt b/file.txt\n--- a/file.txt\n+++ b/file.txt\n" + test.hunk + "\n" + test.line
			chunks, err := splitDiff(diff, 1024)
			if err != nil {
				t.Fatal(err)
			}
			if len(chunks) != 1 {
				t.Fatalf("splitDiff() returned %d chunks, want 1", len(chunks))
			}
			if !strings.Contains(chunks[0], test.want) {
				t.Errorf("split hunk header = %q, want %q", chunks[0], test.want)
			}
		})
	}
}

func TestHasUnresolvedBotComments(t *testing.T) {
	resolver := &gitea.User{Login: "alice"}
	tests := []struct {
		name     string
		comments []gitea.Comment
		want     bool
	}{
		{name: "no bot comments"},
		{name: "unresolved bot finding", comments: []gitea.Comment{{User: gitea.User{Login: "giteye"}, ReviewID: 1, Path: "a.go", Position: 4}}, want: true},
		{name: "resolved thread including bot reply", comments: []gitea.Comment{
			{User: gitea.User{Login: "giteye"}, ReviewID: 1, Path: "a.go", Position: 4, Resolver: resolver},
			{User: gitea.User{Login: "giteye"}, ReviewID: 1, Path: "a.go", Position: 4},
		}},
		{name: "another unresolved bot thread", comments: []gitea.Comment{
			{User: gitea.User{Login: "giteye"}, ReviewID: 1, Path: "a.go", Position: 4, Resolver: resolver},
			{User: gitea.User{Login: "giteye"}, ReviewID: 2, Path: "b.go", Position: 8},
		}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := hasUnresolvedBotComments(test.comments, "giteye"); got != test.want {
				t.Fatalf("hasUnresolvedBotComments() = %v, want %v", got, test.want)
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
