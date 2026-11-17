package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVerifySignature(t *testing.T) {
	body := []byte(`{"action":"review_requested"}`)
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))
	if !Verify("secret", body, sig) {
		t.Fatal("valid signature was rejected")
	}
	if Verify("secret", body, "00") {
		t.Fatal("invalid signature was accepted")
	}
	if Verify("", body, sig) {
		t.Fatal("empty secret was accepted")
	}
}

func TestHandlerRejectsBadSignature(t *testing.T) {
	nextCalled := false
	handler := Handler("secret", 1024, func(http.ResponseWriter, *http.Request, []byte) { nextCalled = true })
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{"action":"review_requested"}`))
	req.Header.Set("X-Gitea-Signature", "bad")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("got status %d, want 401", res.Code)
	}
	if nextCalled {
		t.Fatal("invalid signature reached webhook handler")
	}
}

func TestClassifyStrictTriggers(t *testing.T) {
	event, err := Decode([]byte(`{"action":"created","is_pull":true,"comment":{"id":7782,"body":"точно?","user":{"login":"mullayanov"}},"sender":{"login":"mullayanov"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := Classify(EventPullRequestComment, event, "giteye"); got != TriggerThreadReply {
		t.Fatalf("Gitea PR comment payload classified as %q", got)
	}

	botRequest := Event{Action: "review_requested", RequestedReviewer: User{Login: "giteye"}}
	if got := Classify(EventPullRequestReviewRequest, botRequest, "giteye"); got != TriggerReviewRequest {
		t.Fatalf("review request classified as %q", got)
	}
	other := botRequest
	other.RequestedReviewer = User{Login: "alice"}
	if got := Classify(EventPullRequestReviewRequest, other, "giteye"); got != TriggerIgnore {
		t.Fatalf("other reviewer classified as %q", got)
	}
	push := Event{Action: "synchronized", RequestedReviewer: User{Login: "giteye"}}
	if got := Classify("pull_request_sync", push, "giteye"); got != TriggerIgnore {
		t.Fatalf("push classified as %q", got)
	}
	reply := Event{Action: "created", IsPull: true, Comment: Comment{ID: 42, User: User{Login: "alice"}}}
	if got := Classify(EventPullRequestComment, reply, "giteye"); got != TriggerThreadReply {
		t.Fatalf("PR comment candidate classified as %q", got)
	}
	reply.IsPull = false
	if got := Classify(EventPullRequestComment, reply, "giteye"); got != TriggerIgnore {
		t.Fatalf("non-PR comment classified as %q", got)
	}
	reply.IsPull = true
	reply.Comment.User = User{Login: "giteye"}
	if got := Classify(EventPullRequestComment, reply, "giteye"); got != TriggerIgnore {
		t.Fatalf("bot comment classified as %q", got)
	}
	review := Event{Action: "reviewed", Review: Review{ID: 7, User: User{Login: "alice"}}, Sender: User{Login: "alice"}}
	if got := Classify(EventPullRequestReviewComment, review, "giteye"); got != TriggerIgnore {
		t.Fatalf("submitted review classified as a thread reply: %q", got)
	}
}
