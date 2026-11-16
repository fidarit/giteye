package gitea

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	sdk "gitea.dev/sdk"
)

type Client struct {
	BaseURL string
	Token   string
	api     *sdk.Client
	http    *http.Client
	err     error
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
	c := &Client{BaseURL: strings.TrimRight(base, "/"), Token: token, http: &http.Client{Timeout: 30 * time.Second}}
	c.api, c.err = sdk.NewClient(c.BaseURL, sdk.SetToken(token), sdk.SetHTTPClient(c.http), sdk.SetGiteaVersion("1.28.4"))
	return c
}

func (c *Client) Pull(ctx context.Context, owner, repo string, number int64) (PullRequest, error) {
	if c.err != nil {
		return PullRequest{}, c.err
	}
	p, _, err := c.api.PullRequests.GetPullRequest(ctx, owner, repo, number)
	if err != nil {
		return PullRequest{}, err
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
	if c.err != nil {
		return "", c.err
	}
	diff, _, err := c.api.PullRequests.GetPullRequestDiff(ctx, owner, repo, number, sdk.PullRequestDiffOptions{})
	return string(diff), err
}

func (c *Client) Reviews(ctx context.Context, owner, repo string, number int64) ([]Review, error) {
	if c.err != nil {
		return nil, c.err
	}
	var out []Review
	for page := 1; ; page++ {
		items, _, err := c.api.PullRequests.ListPullReviews(ctx, owner, repo, number, sdk.ListPullReviewsOptions{ListOptions: sdk.ListOptions{Page: page, PageSize: 50}})
		if err != nil {
			return nil, err
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
	if c.err != nil {
		return nil, c.err
	}
	var out []Comment
	for page := 1; ; page++ {
		items, _, err := c.api.Issues.ListIssueComments(ctx, owner, repo, number, sdk.ListIssueCommentOptions{ListOptions: sdk.ListOptions{Page: page, PageSize: 50}})
		if err != nil {
			return nil, err
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

func (c *Client) ReviewComments(ctx context.Context, owner, repo string, number int64) ([]Comment, error) {
	if c.err != nil {
		return nil, c.err
	}
	// The SDK's PullReviewComment currently omits Gitea's in_reply_to field.
	// Keep this typed endpoint read on the SDK; decode the aggregate comment
	// endpoint into the local type so replies can still be matched to their root.
	endpoint := fmt.Sprintf("%s/api/v1/repos/%s/%s/pulls/%d/comments", c.BaseURL, url.PathEscape(owner), url.PathEscape(repo), number)
	var out []Comment
	for page := 1; ; page++ {
		u := endpoint + "?limit=50&page=" + strconv.Itoa(page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "token "+c.Token)
		req.Header.Set("Accept", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			return nil, fmt.Errorf("Gitea API returned HTTP %d", resp.StatusCode)
		}
		var batch []Comment
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&batch)
		resp.Body.Close()
		if decodeErr != nil {
			return nil, decodeErr
		}
		out = append(out, batch...)
		if len(batch) < 50 {
			return out, nil
		}
	}
}

func (c *Client) CreateReview(ctx context.Context, owner, repo string, number int64, commit, event, body string, comments []map[string]any) error {
	if c.err != nil {
		return c.err
	}
	apiComments := make([]sdk.CreatePullReviewComment, 0, len(comments))
	for _, comment := range comments {
		path, _ := comment["path"].(string)
		text, _ := comment["body"].(string)
		position, _ := comment["new_position"].(int)
		oldPosition, _ := comment["old_position"].(int)
		apiComments = append(apiComments, sdk.CreatePullReviewComment{Path: path, Body: text, NewLineNum: int64(position), OldLineNum: int64(oldPosition)})
	}
	_, _, err := c.api.PullRequests.CreatePullReview(ctx, owner, repo, number, sdk.CreatePullReviewOptions{CommitID: commit, State: sdk.ReviewStateType(event), Body: body, Comments: apiComments})
	return err
}

func (c *Client) Reply(ctx context.Context, owner, repo string, number, commentID int64, body string) error {
	if c.err != nil {
		return c.err
	}
	_, _, err := c.api.PullRequests.CreatePullReviewCommentReply(ctx, owner, repo, number, commentID, sdk.CreatePullReviewCommentReplyOptions{Body: body})
	return err
}

func (c *Client) FailureComment(ctx context.Context, owner, repo string, number int64, body string) error {
	if c.err != nil {
		return c.err
	}
	_, _, err := c.api.Issues.CreateIssueComment(ctx, owner, repo, number, sdk.CreateIssueCommentOption{Body: body})
	return err
}

func fromSDKUser(user *sdk.User) User {
	return User{Login: user.UserName, Username: user.UserName}
}
