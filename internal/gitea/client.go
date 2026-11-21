package gitea

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	sdk "gitea.dev/sdk"
)

type Client struct {
	api *sdk.Client
}

type responseLimitTransport struct {
	base     http.RoundTripper
	maxBytes int64
}

func (t responseLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	path := req.URL.Path
	if err == nil && resp != nil && resp.Body != nil && strings.Contains(path, "/pulls/") && strings.Contains(path, "/reviews/") && strings.HasSuffix(path, "/comments") {
		resp.Body = &responseLimitBody{ReadCloser: resp.Body, remaining: t.maxBytes, maxBytes: t.maxBytes}
	}
	return resp, err
}

type responseLimitBody struct {
	io.ReadCloser
	remaining int64
	maxBytes  int64
}

func (b *responseLimitBody) Read(p []byte) (int, error) {
	if b.remaining < 0 {
		return 0, fmt.Errorf("Gitea API response exceeds the %d-byte limit", b.maxBytes)
	}
	if b.remaining == 0 {
		var extra [1]byte
		n, err := b.ReadCloser.Read(extra[:])
		if n > 0 {
			return 0, fmt.Errorf("Gitea API response exceeds the %d-byte limit", b.maxBytes)
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining+1 {
		p = p[:b.remaining+1]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	if b.remaining < 0 {
		return n, fmt.Errorf("Gitea API response exceeds the %d-byte limit", b.maxBytes)
	}
	return n, err
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
	Path      string `json:"path"`
	Position  int    `json:"position"`
	ReviewID  int64  `json:"pull_request_review_id"`
	HTMLURL   string `json:"html_url"`
	CreatedAt string `json:"created_at"`
}

// InlineComment is a validated Gitea review comment on a new-file line.
type InlineComment struct {
	Path string
	Body string
	Line int
}

type Review struct {
	ID          int64  `json:"id"`
	Body        string `json:"body"`
	State       string `json:"state"`
	User        User   `json:"user"`
	SubmittedAt string `json:"submitted_at"`
}

type ReviewEvent string

const (
	ReviewEventComment ReviewEvent = "COMMENT"
	ReviewEventApprove ReviewEvent = "APPROVED"
)

func New(baseURL, token string, maxResponseBytes int) (*Client, error) {
	if maxResponseBytes <= 0 {
		maxResponseBytes = 32 << 20
	}
	httpClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: responseLimitTransport{base: http.DefaultTransport, maxBytes: int64(maxResponseBytes)},
	}
	baseURL = strings.TrimRight(baseURL, "/")
	api, err := sdk.NewClient(baseURL, sdk.SetToken(token), sdk.SetHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("initialize Gitea API client: %w", err)
	}
	return &Client{api: api}, nil
}

func (c *Client) Pull(ctx context.Context, owner, repo string, number int64) (PullRequest, error) {
	p, _, err := c.api.PullRequests.GetPullRequest(ctx, owner, repo, number)
	if err != nil {
		return PullRequest{}, fmt.Errorf("get pull request %s/%s#%d: %w", owner, repo, number, err)
	}
	out := PullRequest{Number: p.Index, Title: p.Title, Body: p.Body, State: string(p.State), HTMLURL: p.HTMLURL}
	if p.Base != nil {
		out.Base.Ref, out.Base.SHA = p.Base.Ref, p.Base.Sha
	}
	if p.Head != nil {
		out.Head.Ref, out.Head.SHA = p.Head.Ref, p.Head.Sha
	}
	for _, reviewer := range p.RequestedReviewers {
		if reviewer != nil {
			out.RequestedReviewers = append(out.RequestedReviewers, fromSDKUser(reviewer))
		}
	}
	return out, nil
}

func (c *Client) Diff(ctx context.Context, owner, repo string, number int64) (string, error) {
	diff, _, err := c.api.PullRequests.GetPullRequestDiff(ctx, owner, repo, number, sdk.PullRequestDiffOptions{})
	if err != nil {
		return "", fmt.Errorf("get pull request diff %s/%s#%d: %w", owner, repo, number, err)
	}
	return string(diff), nil
}

func (c *Client) Reviews(ctx context.Context, owner, repo string, number int64) ([]Review, error) {
	var out []Review
	for page := 1; ; page++ {
		items, _, err := c.api.PullRequests.ListPullReviews(ctx, owner, repo, number, sdk.ListPullReviewsOptions{ListOptions: sdk.ListOptions{Page: page, PageSize: 50}})
		if err != nil {
			return nil, fmt.Errorf("list reviews for %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, item := range items {
			if item == nil {
				continue
			}
			r := Review{ID: item.ID, Body: item.Body, State: string(item.State), SubmittedAt: item.Submitted.Format(time.RFC3339Nano)}
			if item.Reviewer != nil {
				r.User = fromSDKUser(item.Reviewer)
			}
			out = append(out, r)
		}
		if len(items) < 50 {
			return out, nil
		}
	}
}

func (c *Client) IssueComments(ctx context.Context, owner, repo string, number int64) ([]Comment, error) {
	var out []Comment
	for page := 1; ; page++ {
		items, _, err := c.api.Issues.ListIssueComments(ctx, owner, repo, number, sdk.ListIssueCommentOptions{ListOptions: sdk.ListOptions{Page: page, PageSize: 50}})
		if err != nil {
			return nil, fmt.Errorf("list discussion comments for %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, item := range items {
			if item == nil {
				continue
			}
			x := Comment{ID: item.ID, Body: item.Body, HTMLURL: item.HTMLURL, CreatedAt: item.Created.Format(time.RFC3339Nano)}
			if item.Poster != nil {
				x.User = fromSDKUser(item.Poster)
			}
			out = append(out, x)
		}
		if len(items) < 50 {
			return out, nil
		}
	}
}

func (c *Client) ReviewComments(ctx context.Context, owner, repo string, number int64, reviews []Review) ([]Comment, error) {
	var out []Comment
	for _, review := range reviews {
		items, _, err := c.api.PullRequests.ListPullReviewComments(ctx, owner, repo, number, review.ID)
		if err != nil {
			return nil, fmt.Errorf("list inline comments for review %d on %s/%s#%d: %w", review.ID, owner, repo, number, err)
		}
		for _, item := range items {
			if item == nil {
				continue
			}
			comment := Comment{
				ID: item.ID, Body: item.Body, ReviewID: item.ReviewID,
				// Gitea's `position` is the offset in the review diff, not a file line number.
				Path: item.Path, Position: int(item.LineNum),
				CreatedAt: item.Created.Format(time.RFC3339Nano),
			}
			if item.Reviewer != nil {
				comment.User = fromSDKUser(item.Reviewer)
			}
			out = append(out, comment)
		}
	}
	return out, nil
}

func (c *Client) CreateReview(ctx context.Context, owner, repo string, number int64, commit string, event ReviewEvent, body string, comments []InlineComment) error {
	if event != ReviewEventComment && event != ReviewEventApprove {
		return fmt.Errorf("unsupported Gitea review event %q", event)
	}
	apiComments := make([]sdk.CreatePullReviewComment, 0, len(comments))
	for _, comment := range comments {
		apiComments = append(apiComments, sdk.CreatePullReviewComment{Path: comment.Path, Body: comment.Body, NewLineNum: int64(comment.Line)})
	}
	_, _, err := c.api.PullRequests.CreatePullReview(ctx, owner, repo, number, sdk.CreatePullReviewOptions{CommitID: commit, State: sdk.ReviewStateType(event), Body: body, Comments: apiComments})
	if err != nil {
		return fmt.Errorf("create review for %s/%s#%d: %w", owner, repo, number, err)
	}
	return nil
}

func (c *Client) Reply(ctx context.Context, owner, repo string, number, commentID int64, body string) error {
	_, _, err := c.api.PullRequests.CreatePullReviewCommentReply(ctx, owner, repo, number, commentID, sdk.CreatePullReviewCommentReplyOptions{Body: body})
	if err != nil {
		return fmt.Errorf("reply to review comment %d on %s/%s#%d: %w", commentID, owner, repo, number, err)
	}
	return nil
}

func (c *Client) FailureComment(ctx context.Context, owner, repo string, number int64, body string) error {
	_, _, err := c.api.Issues.CreateIssueComment(ctx, owner, repo, number, sdk.CreateIssueCommentOption{Body: body})
	if err != nil {
		return fmt.Errorf("post failure comment on %s/%s#%d: %w", owner, repo, number, err)
	}
	return nil
}

func fromSDKUser(user *sdk.User) User {
	return User{Login: user.UserName, Username: user.UserName}
}
