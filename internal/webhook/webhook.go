package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	EventPullRequestReviewRequest = "pull_request_review_request"
	EventPullRequestComment       = "pull_request_comment"
	EventPullRequestReviewComment = "pull_request_review_comment"
)

// Trigger identifies the work requested by a verified webhook.
type Trigger uint8

const (
	TriggerIgnore Trigger = iota
	TriggerReviewRequest
	TriggerThreadReply
)

func (t Trigger) String() string {
	switch t {
	case TriggerReviewRequest:
		return "review_request"
	case TriggerThreadReply:
		return "thread_reply"
	default:
		return "ignore"
	}
}

type Event struct {
	Action            string      `json:"action"`
	Number            int64       `json:"number"`
	PullRequest       PullRequest `json:"pull_request"`
	Repository        Repository  `json:"repository"`
	RequestedReviewer User        `json:"requested_reviewer"`
	Comment           Comment     `json:"comment"`
	Review            Review      `json:"review"`
	Sender            User        `json:"sender"`
}
type PullRequest struct {
	Number             int64  `json:"number"`
	User               User   `json:"user"`
	Reviewers          []User `json:"reviewers"`
	RequestedReviewers []User `json:"requested_reviewers"`
}
type Repository struct {
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	Owner    User   `json:"owner"`
}
type User struct {
	Login    string `json:"login"`
	Username string `json:"username"`
}
type Comment struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	User      User   `json:"user"`
	InReplyTo int64  `json:"in_reply_to"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	HTMLURL   string `json:"html_url"`
}
type Review struct {
	ID    int64  `json:"id"`
	User  User   `json:"user"`
	Body  string `json:"body"`
	State string `json:"state"`
}

func Verify(secret string, body []byte, signature string) bool {
	if secret == "" || signature == "" {
		return false
	}
	sig, err := hex.DecodeString(strings.TrimSpace(signature))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return hmac.Equal(sig, mac.Sum(nil))
}
func Decode(body []byte) (Event, error) {
	var event Event
	if err := json.Unmarshal(body, &event); err != nil {
		return Event{}, fmt.Errorf("decode Gitea webhook: %w", err)
	}
	return event, nil
}
func Username(u User) string {
	if u.Login != "" {
		return u.Login
	}
	return u.Username
}

// Classify uses the normalized X-Gitea-Event plus specific
// X-Gitea-Event-Type. Review requests have action=review_requested and the
// requested_reviewer object; review replies are PR timeline comments or
// review-thread comments.
func Classify(eventType string, e Event, bot string) Trigger {
	if eventType == EventPullRequestReviewRequest && e.Action == "review_requested" && strings.EqualFold(Username(e.RequestedReviewer), bot) {
		return TriggerReviewRequest
	}
	actor := Username(e.Sender)
	if actor == "" {
		actor = Username(e.Comment.User)
	}
	if actor == "" {
		actor = Username(e.Review.User)
	}
	// Review-comment webhooks describe the submitted review in `review`; the
	// individual reply may need to be resolved from review comments.
	if eventType == EventPullRequestReviewComment && (e.Action == "reviewed" || e.Action == "created") && actor != "" && !strings.EqualFold(actor, bot) {
		return TriggerThreadReply
	}
	if eventType == EventPullRequestComment && e.Action == "created" && !strings.EqualFold(actor, bot) && e.Comment.InReplyTo > 0 {
		return TriggerThreadReply
	}
	return TriggerIgnore
}

func Handler(secret string, limit int64, next func(http.ResponseWriter, *http.Request, []byte)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		defer r.Body.Close()
		b, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if int64(len(b)) > limit {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		if !Verify(secret, b, r.Header.Get("X-Gitea-Signature")) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		next(w, r, b)
	})
}
