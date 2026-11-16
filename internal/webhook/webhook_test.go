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
	botRequest := Event{Action: "review_requested", RequestedReviewer: User{Login: "giteye"}}
	if got := Classify("pull_request_review_request", botRequest, "giteye"); got != "review" {
		t.Fatalf("review request classified as %q", got)
	}
	other := botRequest
	other.RequestedReviewer = User{Login: "alice"}
	if got := Classify("pull_request_review_request", other, "giteye"); got != "ignore" {
		t.Fatalf("other reviewer classified as %q", got)
	}
	push := Event{Action: "synchronized", RequestedReviewer: User{Login: "giteye"}}
	if got := Classify("pull_request_sync", push, "giteye"); got != "ignore" {
		t.Fatalf("push classified as %q", got)
	}
	reply := Event{Action: "created", Comment: Comment{InReplyTo: 42, User: User{Login: "alice"}}}
	if got := Classify("pull_request_review_comment", reply, "giteye"); got != "thread" {
		t.Fatalf("thread reply classified as %q", got)
	}
	reply.Comment.User = User{Login: "giteye"}
	if got := Classify("pull_request_review_comment", reply, "giteye"); got != "ignore" {
		t.Fatalf("bot comment classified as %q", got)
	}
	reviewReply := Event{Action: "reviewed", Review: Review{ID: 7, User: User{Login: "alice"}}, Sender: User{Login: "alice"}}
	if got := Classify("pull_request_review_comment", reviewReply, "giteye"); got != "thread" {
		t.Fatalf("review comment event classified as %q", got)
	}
	reviewReply.Sender = User{Login: "giteye"}
	reviewReply.Review.User = User{Login: "giteye"}
	if got := Classify("pull_request_review_comment", reviewReply, "giteye"); got != "ignore" {
		t.Fatalf("bot review event classified as %q", got)
	}
}
