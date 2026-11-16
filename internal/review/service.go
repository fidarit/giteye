package review

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"

	"giteye/internal/config"
	"giteye/internal/diffvalidate"
	"giteye/internal/gitea"
	"giteye/internal/llm"
	mcpbridge "giteye/internal/mcp"
	"giteye/internal/prompt"
	"giteye/internal/webhook"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Service struct {
	cfg     config.Config
	gitea   *gitea.Client
	ai      *llm.Client
	manager *Manager
	log     *slog.Logger
}

func NewService(c config.Config, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{cfg: c, gitea: gitea.New(c.Gitea.URL, c.Gitea.Token), ai: llm.New(llm.Config{BaseURL: c.AI.BaseURL, APIKey: c.AI.APIKey, Model: c.AI.Model, MaxToolRounds: c.MaxToolRounds}), manager: NewManager(c.MaxConcurrentReviews), log: logger}
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("POST /webhook", webhook.Handler(s.cfg.Webhook.Secret, 2<<20, func(w http.ResponseWriter, r *http.Request, body []byte) { s.accept(w, r, body) }))
	return mux
}

func (s *Service) accept(w http.ResponseWriter, r *http.Request, body []byte) {
	e, err := webhook.Decode(body)
	if err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	kind := webhook.Classify(r.Header.Get("X-Gitea-Event-Type"), e, s.cfg.Gitea.BotUsername)
	if kind == "ignore" {
		w.WriteHeader(http.StatusOK)
		return
	}
	owner, repo := e.Repository.Owner.Login, e.Repository.Name
	if e.Repository.FullName != "" {
		parts := strings.SplitN(e.Repository.FullName, "/", 2)
		if len(parts) == 2 {
			owner, repo = parts[0], parts[1]
		}
	}
	n := e.Number
	if n == 0 {
		n = e.PullRequest.Number
	}
	if owner == "" || repo == "" || n < 1 {
		w.WriteHeader(http.StatusOK)
		return
	}
	key := fmt.Sprintf("%s/%s#%d", owner, repo, n)
	delivery := r.Header.Get("X-Gitea-Delivery")
	if kind == "thread" {
		comments, err := s.gitea.ReviewComments(r.Context(), owner, repo, n)
		if err != nil {
			cause := s.safeError(err)
			s.log.Error("review failed", "pr", key, "cause", cause)
			_, _, _, notify := s.cfg.ForRepo(owner, repo)
			if notify {
				_ = s.gitea.FailureComment(r.Context(), owner, repo, n, failureComment())
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		if e.Comment.InReplyTo == 0 && e.Review.ID > 0 {
			actor := webhook.Username(e.Sender)
			if actor == "" {
				actor = webhook.Username(e.Review.User)
			}
			if c, found := reviewReply(e.Review.ID, actor, comments); found {
				e.Comment = webhook.Comment{ID: c.ID, Body: c.Body, User: webhook.User{Login: userName(c.User)}, InReplyTo: c.InReplyTo, Path: c.Path, Line: c.Line, HTMLURL: c.HTMLURL}
			}
		}
		if _, ok := threadRoot(e.Comment.InReplyTo, comments, s.cfg.Gitea.BotUsername); !ok {
			w.WriteHeader(http.StatusOK)
			return
		}
	}
	s.manager.Start(context.Background(), key, delivery, func(ctx context.Context) {
		if err := s.run(ctx, owner, repo, n, kind, e); err != nil {
			if ctx.Err() != nil {
				return
			}
			cause := s.safeError(err)
			s.log.Error("review failed", "pr", key, "cause", cause)
			_, _, _, notify := s.cfg.ForRepo(owner, repo)
			if notify {
				_ = s.gitea.FailureComment(ctx, owner, repo, n, failureComment())
			}
		}
	})
	w.WriteHeader(http.StatusAccepted)
}

func (s *Service) safeError(err error) string {
	if err == nil {
		return "unknown error"
	}
	cause := strings.TrimSpace(err.Error())
	for _, secret := range []string{s.cfg.Gitea.Token, s.cfg.Webhook.Secret, s.cfg.AI.APIKey, s.cfg.MCP.Token} {
		if len(secret) >= 4 {
			cause = strings.ReplaceAll(cause, secret, "[redacted]")
		}
	}
	if cause == "" {
		return "unknown error"
	}
	return cause
}

func failureComment() string {
	return "Giteye could not complete this review. Check the service logs for the cause, fix it, then retry the review request."
}

func reviewReply(reviewID int64, actor string, comments []gitea.Comment) (gitea.Comment, bool) {
	var found gitea.Comment
	for _, c := range comments {
		if c.ReviewID == reviewID && c.InReplyTo > 0 && (actor == "" || strings.EqualFold(userName(c.User), actor)) && c.CreatedAt >= found.CreatedAt {
			found = c
		}
	}
	return found, found.ID > 0
}

type contextData struct {
	PR                            gitea.PullRequest
	Diff                          string
	Reviews                       []gitea.Review
	IssueComments, ReviewComments []gitea.Comment
}

func (s *Service) load(ctx context.Context, o, r string, n int64) (contextData, error) {
	var d contextData
	var err error
	if d.PR, err = s.gitea.Pull(ctx, o, r, n); err != nil {
		return d, err
	}
	if d.Diff, err = s.gitea.Diff(ctx, o, r, n); err != nil {
		return d, err
	}
	if d.Reviews, err = s.gitea.Reviews(ctx, o, r, n); err != nil {
		return d, err
	}
	if d.IssueComments, err = s.gitea.IssueComments(ctx, o, r, n); err != nil {
		return d, err
	}
	if d.ReviewComments, err = s.gitea.ReviewComments(ctx, o, r, n); err != nil {
		return d, err
	}
	return d, nil
}

func (s *Service) run(ctx context.Context, o, r string, n int64, kind string, e webhook.Event) error {
	d, err := s.load(ctx, o, r, n)
	if err != nil {
		return err
	}
	language, override, addition, _ := s.cfg.ForRepo(o, r)
	system := prompt.Build(override, addition, language)
	user := reviewContext(o, r, d, s.cfg.Gitea.BotUsername)
	var targetID int64
	if kind == "thread" {
		root, ok := threadRoot(e.Comment.InReplyTo, d.ReviewComments, s.cfg.Gitea.BotUsername)
		if !ok {
			return nil
		}
		targetID = root.ID
		user += "\n\nActive bot thread:\n" + threadContext(root, e.Comment, d.ReviewComments)
	}
	var session *mcp.ClientSession
	var tools []llm.Tool
	toolNames := map[string]string{}
	var call func(context.Context, string, map[string]any) (any, error)
	if s.cfg.MCP.URL != "" {
		bridge := mcpbridge.Client{URL: s.cfg.MCP.URL, Token: s.cfg.MCP.Token, AuthHeader: s.cfg.MCP.AuthHeader}
		session, err = bridge.Connect(ctx)
		if err == nil {
			defer session.Close()
			var discovered []mcpbridge.Tool
			discovered, err = mcpbridge.List(ctx, session)
			if err == nil {
				for _, t := range discovered {
					schema, _ := json.Marshal(t.InputSchema)
					if len(schema) == 0 || string(schema) == "null" {
						schema = json.RawMessage(`{"type":"object"}`)
					}
					name := openAIToolName(t.Name, toolNames)
					toolNames[name] = t.Name
					tools = append(tools, llm.Tool{Type: "function", Function: llm.Function{Name: name, Description: t.Description, Parameters: schema}})
				}
			}
		}
		if err != nil {
			s.log.Warn("MCP unavailable; continuing with diff-only review")
			session = nil
			tools = nil
		}
	}
	if session != nil {
		call = func(c context.Context, name string, args map[string]any) (any, error) {
			actual, ok := toolNames[name]
			if !ok {
				return nil, fmt.Errorf("unknown MCP tool")
			}
			callCtx, cancel := context.WithTimeout(c, 90*time.Second)
			defer cancel()
			value, err := mcpbridge.Call(callCtx, session, actual, args)
			if err != nil {
				s.log.Warn("MCP tool call failed; continuing review")
			}
			return value, err
		}
	}
	result, err := s.ai.Review(ctx, system, user, tools, call)
	if err != nil {
		return err
	}
	if kind == "thread" {
		comments := diffvalidate.GiteaComments(d.Diff, toDiffComments(result.Comments))
		if len(comments) > 0 {
			if err := s.gitea.CreateReview(ctx, o, r, n, d.PR.Head.SHA, "COMMENT", result.Summary, comments); err != nil {
				return err
			}
		}
		threadReply := result.Summary
		if result.ThreadReply != nil && strings.TrimSpace(*result.ThreadReply) != "" {
			threadReply = *result.ThreadReply
		}
		return s.gitea.Reply(ctx, o, r, n, targetID, threadReply)
	}
	comments := diffvalidate.GiteaComments(d.Diff, toDiffComments(result.Comments))
	event := "COMMENT"
	if result.Event == "APPROVE" && len(comments) == 0 {
		event = "APPROVED"
	}
	return s.gitea.CreateReview(ctx, o, r, n, d.PR.Head.SHA, event, result.Summary, comments)
}

func openAIToolName(name string, used map[string]string) string {
	var b strings.Builder
	for _, r := range name {
		if r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
		if b.Len() >= 48 {
			break
		}
	}
	base := strings.Trim(b.String(), "_")
	if base == "" {
		base = "mcp_tool"
	}
	candidate := base
	for suffix := 2; used[candidate] != "" && used[candidate] != name; suffix++ {
		candidate = fmt.Sprintf("%s_%d", base, suffix)
	}
	return candidate
}

func toDiffComments(in []llm.Comment) []diffvalidate.Comment {
	out := make([]diffvalidate.Comment, 0, len(in))
	for _, c := range in {
		out = append(out, diffvalidate.Comment{Path: c.Path, Line: c.Line, Body: c.Body})
	}
	return out
}
func reviewContext(owner, repo string, d contextData, bot string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Repository: %s/%s\nPull request: #%d %s\nURL: %s\nBase: %s (%s)\nHead: %s (%s)\nDescription:\n%s\n\nDiff:\n%s\n\nPrior reviews and discussion (use as history; do not repeat resolved findings blindly):\n", owner, repo, d.PR.Number, d.PR.Title, d.PR.HTMLURL, d.PR.Base.Ref, d.PR.Base.SHA, d.PR.Head.Ref, d.PR.Head.SHA, d.PR.Body, d.Diff)
	for _, r := range d.Reviews {
		fmt.Fprintf(&b, "Review by %s [%s]: %s\n", userName(r.User), r.State, r.Body)
	}
	comments := append(append([]gitea.Comment{}, d.IssueComments...), d.ReviewComments...)
	sort.SliceStable(comments, func(i, j int) bool { return comments[i].CreatedAt < comments[j].CreatedAt })
	for _, c := range comments {
		fmt.Fprintf(&b, "Comment #%d by %s%s: %s\n", c.ID, userName(c.User), botSuffix(c.User, bot), c.Body)
	}
	return b.String()
}
func botSuffix(u gitea.User, bot string) string {
	if strings.EqualFold(userName(u), bot) {
		return " (Giteye)"
	}
	return ""
}
func userName(u gitea.User) string {
	if u.Login != "" {
		return u.Login
	}
	return u.Username
}

func threadRoot(parent int64, comments []gitea.Comment, bot string) (gitea.Comment, bool) {
	byID := map[int64]gitea.Comment{}
	for _, c := range comments {
		byID[c.ID] = c
	}
	seen := map[int64]bool{}
	for parent > 0 {
		if seen[parent] {
			return gitea.Comment{}, false
		}
		seen[parent] = true
		c, ok := byID[parent]
		if !ok {
			return gitea.Comment{}, false
		}
		if strings.EqualFold(userName(c.User), bot) {
			return c, true
		}
		parent = c.InReplyTo
	}
	return gitea.Comment{}, false
}
func threadContext(root gitea.Comment, reply webhook.Comment, comments []gitea.Comment) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Root comment #%d by Giteye: %s\n", root.ID, root.Body)
	seen := map[int64]bool{root.ID: true}
	var thread []gitea.Comment
	for changed := true; changed; {
		changed = false
		for _, c := range comments {
			if !seen[c.ID] && seen[c.InReplyTo] {
				seen[c.ID] = true
				changed = true
				thread = append(thread, c)
			}
		}
	}
	sort.SliceStable(thread, func(i, j int) bool { return thread[i].CreatedAt < thread[j].CreatedAt })
	foundReply := false
	for _, c := range thread {
		marker := ""
		if c.ID == reply.ID {
			marker = " (current reply)"
			foundReply = true
		}
		fmt.Fprintf(&b, "Reply #%d by %s%s: %s\n", c.ID, userName(c.User), marker, c.Body)
	}
	if !foundReply {
		fmt.Fprintf(&b, "Current reply by %s: %s\n", webhook.Username(reply.User), reply.Body)
	}
	return b.String()
}
