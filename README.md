# Giteye

Giteye is a self-hosted AI reviewer for Gitea. It runs a full review only when its account is explicitly requested as a pull request reviewer. A reply to a bot review thread can trigger a contextual response and a check against the current diff. Pushes and ordinary pull request open/sync events do not start reviews.

## Gitea 28.4 setup

Create a **Gitea** webhook at repository settings (`/:owner/:repo/settings/hooks`), or install an organization or system webhook to cover multiple repositories. Use:

- Target URL: `https://giteye.example.com/webhook`
- HTTP method: `POST`
- Content type: `application/json`
- Secret: the same value as `webhook.secret` / `WEBHOOK_SECRET`
- Trigger On: **Custom Events**, selecting **Pull Request Review Request**, **Pull Request Comments**, and **Pull Request Review Comments**

The corresponding Gitea event types are `pull_request_review_request`, `pull_request_comment`, and `pull_request_review_comment`. The review request event's `review_requested` action and `requested_reviewer` identify the targeted reviewer. `pull_request_sync` is deliberately not enabled. Gitea signs the raw body with HMAC-SHA256 in `X-Gitea-Signature`; Giteye requires and verifies that header before decoding the payload.

Create a dedicated bot account and request it as a reviewer on PRs. Give it repository read access and permission to create pull reviews and issue comments. For a classic access token, the minimal scopes are `read:repository`, `write:repository`, `read:issue`, and `write:issue`; the bot account must also have access to each covered repository. Gitea uses `Authorization: token <token>` for API calls.

Gitea 28.4 creates reviews through `POST /api/v1/repos/{owner}/{repo}/pulls/{index}/reviews`. Giteye maps an absolute new-file line from model output to its unified-diff position and sends that value as `new_position`; comments with no matching current diff line are dropped. Model `APPROVE` maps to Gitea's API event `APPROVED`, while all other formal reviews use `COMMENT`.

## Configuration

Copy `config.example.yaml` to `config.yaml` and fill in credentials. Secret values and service URLs can be overridden with environment variables; `.env.example` lists them. YAML options also include a per-repository map keyed by `owner/repo`, which can override review language, prompts, and failure notification.

The default review language is Russian. `system_prompt_override` replaces the default system prompt; `system_prompt_addition` appends instructions. `notify_on_failure` defaults to `true` and posts a short PR comment on hard failure. It never includes error details or credentials. `max_tool_rounds` defaults to 6 and `max_concurrent_reviews` defaults to 4.

MCP is optional. Leave `mcp.url` empty for diff and Gitea API context only. Any MCP server exposing Streamable HTTP can be used; Giteye discovers tools with `tools/list`, presents their schemas to the model, and calls selected tools by their discovered names. It has no gitea-mcp-specific tool names or assumptions. The official `gitea-mcp` v1.13.x is one possible server, not a dependency.

When `mcp.token` is set, Giteye sends it as a Bearer token in the `Authorization` header by default. Set `mcp.auth_header` to another header name to send the token value there unchanged.

## Run

```sh
go run ./cmd/giteye -config config.yaml
```

`GET /healthz` is a liveness endpoint. `POST /webhook` accepts signed Gitea JSON webhooks.

Build and run with Docker:

```sh
docker build -t giteye .
docker run --rm -p 8080:8080 --env-file .env giteye
```

To load optional YAML settings with `docker run`, add `-v "$PWD/config.yaml:/app/config.yaml:ro"`.

## Docker Compose

```sh
cp .env.example .env
cp config.example.yaml config.yaml
```

Fill in the Gitea URL and token, webhook secret, and AI settings in `.env`, then start the service. YAML configuration is optional; when `config.yaml` is present, it is loaded first and environment values override it:

```sh
docker compose up -d
```

## Behavior

- Review request delivery starts one full review. A repeated delivery ID is suppressed; a newer relevant request for that PR cancels its in-flight work. Reviews across all PRs share the configured concurrency limit.
- A push alone is ignored. A subsequent explicit reviewer request starts a fresh full review, with earlier bot findings and PR discussion included in context.
- Replies are handled only when the parent chain reaches a comment written by Giteye. The model sees that thread, prior discussion, PR metadata, and the current diff. It can close politely, explain a finding, or re-check changed code.
- Inline comments are validated independently, so a stale or invalid model line cannot reject the rest of the review.
- MCP connection or discovery failures are logged as warnings and the review continues without tools.

## Development

```sh
go test ./...
go build ./cmd/giteye
```
