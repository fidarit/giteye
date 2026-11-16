package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	BaseURL, Token string
	HTTP           *http.Client
}
type User struct {
	Login    string `json:"login"`
	Username string `json:"username"`
}
type PullRequest struct {
	Number  int64  `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
	Base    struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
	Head struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	RequestedReviewers []User `json:"requested_reviewers"`
}
type Comment struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	User      User   `json:"user"`
	InReplyTo int64  `json:"in_reply_to"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Position  int    `json:"position"`
	ReviewID  int64  `json:"pull_request_review_id"`
	HTMLURL   string `json:"html_url"`
	CreatedAt string `json:"created_at"`
}
type Review struct {
	ID          int64  `json:"id"`
	Body        string `json:"body"`
	State       string `json:"state"`
	User        User   `json:"user"`
	SubmittedAt string `json:"submitted_at"`
}

func New(base, token string) *Client {
	return &Client{BaseURL: strings.TrimRight(base, "/"), Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
}
func (c *Client) path(owner, repo string, n int64, suffix string) string {
	return fmt.Sprintf("%s/api/v1/repos/%s/%s/pulls/%d%s", c.BaseURL, url.PathEscape(owner), url.PathEscape(repo), n, suffix)
}
func (c *Client) do(ctx context.Context, method, endpoint string, body any, out any) error {
	var b io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		b = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, b)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	h := c.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	resp, err := h.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Gitea API returned HTTP %d", resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}
func (c *Client) Pull(ctx context.Context, o, r string, n int64) (PullRequest, error) {
	var p PullRequest
	err := c.do(ctx, "GET", c.path(o, r, n, ""), nil, &p)
	return p, err
}
func (c *Client) Diff(ctx context.Context, o, r string, n int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.path(o, r, n, ".diff"), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "token "+c.Token)
	req.Header.Set("Accept", "text/plain")
	h := c.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	resp, err := h.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("Gitea API returned HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	return string(b), err
}
func (c *Client) paged(ctx context.Context, endpoint string, out any) error {
	var all []json.RawMessage
	for page := 1; ; page++ {
		u := endpoint + "?limit=50&page=" + strconv.Itoa(page)
		var batch []json.RawMessage
		if err := c.do(ctx, "GET", u, nil, &batch); err != nil {
			return err
		}
		all = append(all, batch...)
		if len(batch) < 50 {
			break
		}
	}
	b, _ := json.Marshal(all)
	return json.Unmarshal(b, out)
}
func (c *Client) Reviews(ctx context.Context, o, r string, n int64) ([]Review, error) {
	var x []Review
	err := c.paged(ctx, c.path(o, r, n, "/reviews"), &x)
	return x, err
}
func (c *Client) IssueComments(ctx context.Context, o, r string, n int64) ([]Comment, error) {
	var x []Comment
	err := c.paged(ctx, c.issuePath(o, r, n)+"/comments", &x)
	return x, err
}
func (c *Client) ReviewComments(ctx context.Context, o, r string, n int64) ([]Comment, error) {
	var x []Comment
	err := c.paged(ctx, c.path(o, r, n, "/comments"), &x)
	return x, err
}
func (c *Client) issuePath(o, r string, n int64) string {
	return fmt.Sprintf("%s/api/v1/repos/%s/%s/issues/%d", c.BaseURL, url.PathEscape(o), url.PathEscape(r), n)
}

// CreateReview uses Gitea 28.x CreatePullReviewComment.new_position. The caller
// maps an absolute new-file line to its unified-diff position and sets
// old_position to zero; Gitea's API event for model APPROVE is APPROVED.
func (c *Client) CreateReview(ctx context.Context, o, r string, n int64, commit, event, body string, comments []map[string]any) error {
	return c.do(ctx, "POST", c.path(o, r, n, "/reviews"), map[string]any{"commit_id": commit, "event": event, "body": body, "comments": comments}, nil)
}
func (c *Client) Reply(ctx context.Context, o, r string, n, commentID int64, body string) error {
	return c.do(ctx, "POST", c.path(o, r, n, "/comments/"+strconv.FormatInt(commentID, 10)+"/replies"), map[string]string{"body": body}, nil)
}
func (c *Client) FailureComment(ctx context.Context, o, r string, n int64, body string) error {
	return c.do(ctx, "POST", c.issuePath(o, r, n)+"/comments", map[string]string{"body": body}, nil)
}
