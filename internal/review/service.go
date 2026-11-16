package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
)

var errNotBotThread = errors.New("comment does not belong to a Giteye review thread")

type Service struct {
	cfg     config.Config
	gitea   *gitea.Client
	ai      *llm.Client
	manager *Manager
	log     *slog.Logger
	ctx     context.Context
	cancel  context.CancelFunc
}

func NewService(c config.Config, logger *slog.Logger) (*Service, error) {
	if logger == nil {
		logger = slog.Default()
	}
	giteaClient, err := gitea.New(c.Gitea.URL, c.Gitea.Token)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		cfg:     c,
		gitea:   giteaClient,
		ai:      llm.New(llm.Config{BaseURL: c.AI.BaseURL, APIKey: c.AI.APIKey, Model: c.AI.Model, MaxToolRounds: c.MaxToolRounds}),
		manager: NewManager(c.MaxConcurrentReviews),
		log:     logger,
		ctx:     ctx,
		cancel:  cancel,
	}, nil
}

func (s *Service) Shutdown(ctx context.Context) error {
	s.cancel()
	return s.manager.Shutdown(ctx)
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("POST /webhook", webhook.Handler(s.cfg.Webhook.Secret, webhook.MaxPayloadBytes, func(w http.ResponseWriter, r *http.Request, body []byte) { s.accept(w, r, body) }))
	return mux
}

func (s *Service) accept(w http.ResponseWriter, r *http.Request, body []byte) {
	e, err := webhook.Decode(body)
	if err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	kind := webhook.Classify(r.Header.Get("X-Gitea-Event-Type"), e, s.cfg.Gitea.BotUsername)
	if kind == webhook.TriggerIgnore {
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
	if delivery == "" {
		digest := sha256.Sum256(body)
		delivery = hex.EncodeToString(digest[:])
	}
	scheduled := s.manager.Start(s.ctx, key, delivery, func(ctx context.Context) {
		started := time.Now()
		s.log.Info("review started", "pr", key, "kind", kind.String(), "model", s.cfg.AI.Model)
		usage, err := s.run(ctx, owner, repo, n, kind, e)
		attrs := []any{"pr", key, "kind", kind.String(), "model", s.cfg.AI.Model, "duration", time.Since(started).Round(time.Millisecond), "tool_calls", usage.ToolCalls, "tool_rounds", usage.ToolRounds, "usage_available", usage.Available}
		if usage.Available {
			attrs = append(attrs,
				"input_tokens", usage.InputTokens,
				"output_tokens", usage.OutputTokens,
				"total_tokens", usage.TotalTokens,
				"cached_input_tokens", usage.CachedInput,
			)
		}
		if err == nil {
			s.log.Info("review finished", append(attrs, "status", "completed")...)
			return
		}
		if ctx.Err() != nil {
			s.log.Info("review finished", append(attrs, "status", "canceled")...)
			return
		}
		if errors.Is(err, errNotBotThread) {
			s.log.Info("review finished", append(attrs, "status", "ignored")...)
			return
		}
		cause := s.safeError(err)
		s.log.Error("review finished", append(attrs, "status", "failed", "cause", cause)...)
		if s.cfg.ForRepo(owner, repo).NotifyOnFailure {
			if commentErr := s.gitea.FailureComment(ctx, owner, repo, n, failureComment()); commentErr != nil {
				s.log.Error("failure comment could not be posted", "pr", key, "cause", s.safeError(commentErr))
			}
		}
	})
	if !scheduled {
		s.log.Debug("review event was not scheduled", "pr", key)
	}
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

func (s *Service) load(ctx context.Context, o, r string, n int64, includeDiscussion bool) (contextData, error) {
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
	if includeDiscussion {
		if d.IssueComments, err = s.gitea.IssueComments(ctx, o, r, n); err != nil {
			return d, err
		}
	}
	if d.ReviewComments, err = s.gitea.ReviewComments(ctx, o, r, n, d.Reviews); err != nil {
		return d, err
	}
	return d, nil
}

func (s *Service) run(ctx context.Context, o, r string, n int64, kind webhook.Trigger, e webhook.Event) (llm.Usage, error) {
	includeHistory := kind != webhook.TriggerThreadReply
	d, err := s.load(ctx, o, r, n, includeHistory)
	if err != nil {
		return llm.Usage{}, err
	}
	settings := s.cfg.ForRepo(o, r)
	system := prompt.Build(settings.SystemPromptOverride, settings.SystemPromptAddition, settings.Language)
	user := reviewContext(o, r, d, s.cfg.Gitea.BotUsername, includeHistory)
	var targetID int64
	if kind == webhook.TriggerThreadReply {
		if e.Comment.InReplyTo == 0 && e.Review.ID > 0 {
			actor := webhook.Username(e.Sender)
			if actor == "" {
				actor = webhook.Username(e.Review.User)
			}
			if comment, found := reviewReply(e.Review.ID, actor, d.ReviewComments); found {
				e.Comment = toWebhookComment(comment)
			}
		}
		root, ok := threadRoot(e.Comment.InReplyTo, d.ReviewComments, s.cfg.Gitea.BotUsername)
		if !ok {
			return llm.Usage{}, errNotBotThread
		}
		targetID = root.ID
		user += "\n\nActive bot thread:\n" + threadContext(root, e.Comment, d.ReviewComments)
	}
	session, tools, toolNames, err := s.discoverMCPTools(ctx)
	if err != nil {
		s.log.Warn("MCP unavailable; continuing with diff-only review", "cause", s.safeError(err))
		session, tools, toolNames = nil, nil, nil
	}
	if session != nil {
		defer func() {
			if err := session.Close(); err != nil {
				s.log.Warn("close MCP session failed", "cause", s.safeError(err))
			}
		}()
	}
	var call func(context.Context, string, map[string]any) (any, error)
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
				s.log.Warn("MCP tool call failed", "tool", actual, "cause", s.safeError(err))
			}
			return value, err
		}
	}
	result, err := s.ai.Review(ctx, system, user, tools, call)
	if err != nil {
		return result.Usage, err
	}
	if kind == webhook.TriggerThreadReply {
		comments := diffvalidate.GiteaComments(d.Diff, toDiffComments(result.Comments))
		threadReply := result.Summary
		if result.ThreadReply != nil && strings.TrimSpace(*result.ThreadReply) != "" {
			threadReply = *result.ThreadReply
		}
		if strings.TrimSpace(threadReply) == "" {
			return result.Usage, errors.New("AI returned an empty thread reply")
		}
		if len(comments) > 0 {
			if err := s.gitea.CreateReview(ctx, o, r, n, d.PR.Head.SHA, gitea.ReviewEventComment, result.Summary, comments); err != nil {
				return result.Usage, err
			}
		}
		return result.Usage, s.gitea.Reply(ctx, o, r, n, targetID, threadReply)
	}
	comments := diffvalidate.GiteaComments(d.Diff, toDiffComments(result.Comments))
	if len(comments) == 0 && strings.TrimSpace(result.Summary) == "" {
		return result.Usage, errors.New("AI returned neither a review summary nor valid inline comments")
	}
	event := gitea.ReviewEventComment
	if result.Event == "APPROVE" && len(comments) == 0 {
		event = gitea.ReviewEventApprove
	}
	return result.Usage, s.gitea.CreateReview(ctx, o, r, n, d.PR.Head.SHA, event, result.Summary, comments)
}

func (s *Service) discoverMCPTools(ctx context.Context) (*mcpbridge.Session, []llm.Tool, map[string]string, error) {
	if s.cfg.MCP.URL == "" {
		return nil, nil, nil, nil
	}
	bridge := mcpbridge.Client{URL: s.cfg.MCP.URL, Token: s.cfg.MCP.Token, AuthHeader: s.cfg.MCP.AuthHeader}
	session, err := bridge.Connect(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	discovered, err := mcpbridge.List(ctx, session)
	if err != nil {
		closeErr := session.Close()
		return nil, nil, nil, errors.Join(fmt.Errorf("list MCP tools: %w", err), closeErr)
	}
	if len(discovered) == 0 {
		if err := session.Close(); err != nil {
			return nil, nil, nil, fmt.Errorf("close MCP session: %w", err)
		}
		return nil, nil, nil, nil
	}

	tools := make([]llm.Tool, 0, len(discovered))
	toolNames := make(map[string]string, len(discovered))
	for _, discoveredTool := range discovered {
		schema, err := json.Marshal(discoveredTool.InputSchema)
		if err != nil {
			closeErr := session.Close()
			return nil, nil, nil, errors.Join(fmt.Errorf("encode schema for MCP tool %q: %w", discoveredTool.Name, err), closeErr)
		}
		if len(schema) == 0 || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		name := openAIToolName(discoveredTool.Name, toolNames)
		toolNames[name] = discoveredTool.Name
		tools = append(tools, llm.Tool{
			Type: "function",
			Function: llm.Function{
				Name: name, Description: discoveredTool.Description,
				Parameters: schema,
			},
		})
	}
	return session, tools, toolNames, nil
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
func reviewContext(owner, repo string, d contextData, bot string, includeHistory bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Repository: %s/%s\nPull request: #%d %s\nURL: %s\nBase: %s (%s)\nHead: %s (%s)\nDescription:\n%s\n\nDiff:\n%s\n", owner, repo, d.PR.Number, d.PR.Title, d.PR.HTMLURL, d.PR.Base.Ref, d.PR.Base.SHA, d.PR.Head.Ref, d.PR.Head.SHA, d.PR.Body, d.Diff)
	if !includeHistory {
		return b.String()
	}
	b.WriteString("\nPrior reviews and discussion (use as history; do not repeat resolved findings blindly):\n")
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

func toWebhookComment(comment gitea.Comment) webhook.Comment {
	return webhook.Comment{
		ID: comment.ID, Body: comment.Body,
		User:      webhook.User{Login: userName(comment.User)},
		InReplyTo: comment.InReplyTo, Path: comment.Path, Line: comment.Line,
		HTMLURL: comment.HTMLURL,
	}
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
	children := make(map[int64][]gitea.Comment, len(comments))
	for _, comment := range comments {
		if comment.InReplyTo > 0 {
			children[comment.InReplyTo] = append(children[comment.InReplyTo], comment)
		}
	}
	seen := map[int64]bool{root.ID: true}
	queue := []int64{root.ID}
	var thread []gitea.Comment
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, comment := range children[parent] {
			if seen[comment.ID] {
				continue
			}
			seen[comment.ID] = true
			thread = append(thread, comment)
			queue = append(queue, comment.ID)
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
