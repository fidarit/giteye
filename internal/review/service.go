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
	c.Limits = c.Limits.WithDefaults()
	if logger == nil {
		logger = slog.Default()
	}
	giteaClient, err := gitea.New(c.Gitea.URL, c.Gitea.Token, c.Limits.GiteaReviewCommentsBytes)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		cfg:   c,
		gitea: giteaClient,
		ai: llm.New(llm.Config{
			BaseURL: c.AI.BaseURL, APIKey: c.AI.APIKey, Model: c.AI.Model,
			MaxToolRounds: c.MaxToolRounds, MaxToolResultBytes: c.Limits.MCPToolResultBytes,
			LogRequests: c.LogAPIRequests, Logger: logger,
		}),
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
	mux.Handle("POST /webhook", webhook.Handler(s.cfg.Webhook.Secret, int64(s.cfg.Limits.WebhookPayloadBytes), func(w http.ResponseWriter, r *http.Request, body []byte) { s.accept(w, r, body) }))
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
		if e.IsPull && e.Comment.ID > 0 {
			s.log.Info("PR comment webhook ignored", "event_type", r.Header.Get("X-Gitea-Event-Type"), "action", e.Action, "comment_id", e.Comment.ID)
		}
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
		if secret != "" {
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
	omittedHistory := 0
	omittedHistoryBytes := 0
	if len(user) > s.cfg.Limits.ReviewContextBytes && includeHistory {
		user, omittedHistory, omittedHistoryBytes = reviewContextWithinLimit(o, r, d, s.cfg.Gitea.BotUsername, s.cfg.Limits.ReviewContextBytes)
		if omittedHistory > 0 {
			s.log.Warn("omitted review history entries to fit context limit", "pr", fmt.Sprintf("%s/%s#%d", o, r, n), "count", omittedHistory, "bytes", omittedHistoryBytes)
		}
	}
	var targetID int64
	if kind == webhook.TriggerThreadReply {
		root, reply, ok := resolveThreadReply(e.Comment, webhook.Username(e.Sender), d.ReviewComments, s.cfg.Gitea.BotUsername)
		if !ok {
			return llm.Usage{}, errNotBotThread
		}
		targetID = root.ID
		user += "\n\nActive bot thread:\n" + threadContext(root, reply, d.ReviewComments)
	}
	if len(user) > s.cfg.Limits.ReviewContextBytes {
		return llm.Usage{}, fmt.Errorf("review context is %d bytes and exceeds the %d-byte safety limit", len(user), s.cfg.Limits.ReviewContextBytes)
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
		if err := s.gitea.Reply(ctx, o, r, n, targetID, threadReply); err != nil {
			return result.Usage, err
		}
		if result.ResolveThread {
			if err := s.gitea.ResolveReviewComment(ctx, o, r, targetID); err != nil {
				return result.Usage, err
			}
		}
		return result.Usage, nil
	}
	comments := diffvalidate.GiteaComments(d.Diff, toDiffComments(result.Comments))
	if err := validateReviewOutput(result.Event, result.Summary, comments); err != nil {
		return result.Usage, err
	}
	event := gitea.ReviewEventComment
	if result.Event == "APPROVE" && len(comments) == 0 {
		event = gitea.ReviewEventApprove
	}
	return result.Usage, s.gitea.CreateReview(ctx, o, r, n, d.PR.Head.SHA, event, result.Summary, comments)
}

func validateReviewOutput(event, summary string, comments []gitea.InlineComment) error {
	if len(comments) == 0 && strings.TrimSpace(summary) == "" && event != "APPROVE" {
		return errors.New("AI returned neither a review summary nor valid inline comments")
	}
	return nil
}

func (s *Service) discoverMCPTools(ctx context.Context) (*mcpbridge.Session, []llm.Tool, map[string]string, error) {
	if s.cfg.MCP.URL == "" {
		return nil, nil, nil, nil
	}
	bridge := mcpbridge.Client{
		URL: s.cfg.MCP.URL, Token: s.cfg.MCP.Token, AuthHeader: s.cfg.MCP.AuthHeader,
		LogRequests: s.cfg.LogAPIRequests, Logger: s.log,
	}
	session, err := bridge.Connect(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	limits := s.cfg.Limits
	discovered, err := mcpbridge.List(ctx, session, limits.MCPToolListPages, limits.MCPDiscoveredTools)
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
	allowedTools := make(map[string]struct{}, len(s.cfg.MCP.AllowedTools))
	for _, name := range s.cfg.MCP.AllowedTools {
		allowedTools[name] = struct{}{}
	}

	tools := make([]llm.Tool, 0, len(discovered))
	toolNames := make(map[string]string, len(discovered))
	var schemaBytes int
	for _, discoveredTool := range discovered {
		_, explicitlyAllowed := allowedTools[discoveredTool.Name]
		if !discoveredTool.ReadOnly && !explicitlyAllowed {
			s.log.Warn("skipping MCP tool without read-only annotation", "tool", discoveredTool.Name)
			continue
		}
		schema, err := json.Marshal(discoveredTool.InputSchema)
		if err != nil {
			closeErr := session.Close()
			return nil, nil, nil, errors.Join(fmt.Errorf("encode schema for MCP tool %q: %w", discoveredTool.Name, err), closeErr)
		}
		if len(schema) == 0 || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		if len(schema) > limits.MCPToolSchemaBytes || schemaBytes+len(schema) > limits.MCPTotalSchemaBytes || len(tools) >= limits.MCPExposedTools {
			s.log.Warn("skipping MCP tool that exceeds schema limits", "tool", discoveredTool.Name)
			continue
		}
		schemaBytes += len(schema)
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
	if len(tools) == 0 {
		if err := session.Close(); err != nil {
			return nil, nil, nil, fmt.Errorf("close MCP session: %w", err)
		}
		return nil, nil, nil, nil
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

func reviewContextWithinLimit(owner, repo string, d contextData, bot string, limit int) (string, int, int) {
	d.Reviews = append([]gitea.Review(nil), d.Reviews...)
	d.IssueComments = append([]gitea.Comment(nil), d.IssueComments...)
	d.ReviewComments = append([]gitea.Comment(nil), d.ReviewComments...)
	omittedCount, omittedBytes := 0, 0
	for {
		user := reviewContext(owner, repo, d, bot, true)
		if len(user) <= limit {
			return user, omittedCount, omittedBytes
		}

		// Drop the largest history entry first, preserving shorter discussion
		// that may still help review the current changes.
		group, index, largest := -1, -1, -1
		for i, review := range d.Reviews {
			if len(review.Body) > largest {
				group, index, largest = 0, i, len(review.Body)
			}
		}
		for i, comment := range d.IssueComments {
			if len(comment.Body) > largest {
				group, index, largest = 1, i, len(comment.Body)
			}
		}
		for i, comment := range d.ReviewComments {
			if len(comment.Body) > largest {
				group, index, largest = 2, i, len(comment.Body)
			}
		}
		if group < 0 {
			return reviewContext(owner, repo, d, bot, false), omittedCount, omittedBytes
		}
		switch group {
		case 0:
			omittedBytes += len(d.Reviews[index].Body)
			d.Reviews = append(d.Reviews[:index], d.Reviews[index+1:]...)
		case 1:
			omittedBytes += len(d.IssueComments[index].Body)
			d.IssueComments = append(d.IssueComments[:index], d.IssueComments[index+1:]...)
		case 2:
			omittedBytes += len(d.ReviewComments[index].Body)
			d.ReviewComments = append(d.ReviewComments[:index], d.ReviewComments[index+1:]...)
		}
		omittedCount++
	}
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

func resolveThreadReply(eventComment webhook.Comment, actor string, comments []gitea.Comment, bot string) (gitea.Comment, gitea.Comment, bool) {
	var reply gitea.Comment
	for _, c := range comments {
		if c.ID == eventComment.ID && (actor == "" || strings.EqualFold(userName(c.User), actor)) {
			reply = c
			break
		}
	}
	if reply.ID == 0 || reply.ReviewID == 0 || reply.Path == "" || reply.Position <= 0 {
		return gitea.Comment{}, gitea.Comment{}, false
	}
	var root gitea.Comment
	for _, c := range comments {
		if c.ID == reply.ID || c.ReviewID != reply.ReviewID || c.Path != reply.Path || c.Position != reply.Position || !strings.EqualFold(userName(c.User), bot) {
			continue
		}
		if c.CreatedAt != "" && reply.CreatedAt != "" && c.CreatedAt > reply.CreatedAt {
			continue
		}
		if root.ID == 0 || (c.CreatedAt != "" && (root.CreatedAt == "" || c.CreatedAt < root.CreatedAt)) || (c.CreatedAt == root.CreatedAt && c.ID < root.ID) {
			root = c
		}
	}
	return root, reply, root.ID != 0
}

func threadContext(root, reply gitea.Comment, comments []gitea.Comment) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Root comment #%d by Giteye: %s\n", root.ID, root.Body)
	var thread []gitea.Comment
	for _, comment := range comments {
		if comment.ID == root.ID || comment.ReviewID != root.ReviewID || comment.Path != root.Path || comment.Position != root.Position {
			continue
		}
		thread = append(thread, comment)
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
		fmt.Fprintf(&b, "Current reply by %s: %s\n", userName(reply.User), reply.Body)
	}
	return b.String()
}
