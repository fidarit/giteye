# Giteye

Giteye is a self-hosted AI reviewer for Gitea. It reviews a pull request only after its bot account is requested as a reviewer. It can also respond when someone replies to a comment in an active Giteye review thread. Pushes and pull request open/sync events do not start reviews.

## Requirements

- Gitea **1.27.0 or newer**. Giteye uses the API endpoint for replying to pull request review comments, added in [Gitea 1.27.0](https://blog.gitea.com/release-of-1.27.0/).
- A reachable Gitea instance and a bot account with repository read access and permission to create pull reviews and issue comments.
- An OpenAI-compatible Chat Completions endpoint, API key, and model.
- Docker Compose, or Go **1.27** to build and run from source.

## Quick start with Docker Compose

```sh
cp .env.example .env
cp config.example.yaml config.yaml
```

Set `GITEA_URL`, `GITEA_TOKEN`, `GITEA_BOT_USERNAME`, `WEBHOOK_SECRET`, `AI_BASE_URL`, `AI_API_KEY`, and `AI_MODEL` in `.env`. Then run:

```sh
docker compose up -d
docker compose logs -f giteye
```

Compose mounts `config.yaml` into the container. To run with environment variables only, remove the `config.yaml` volume entry from `compose.yaml`. At the application level, the YAML file is optional.

The service listens on port `8080` by default. Configure Gitea to send webhooks to `https://<your-host>/webhook`; `GET /healthz` is the liveness endpoint.

## Gitea setup

Create a Gitea webhook at repository settings (`/:owner/:repo/settings/hooks`), or use an organization or system webhook to cover multiple repositories:

- Target URL: `https://<your-host>/webhook`
- HTTP method: `POST`
- Content type: `application/json`
- Secret: the value of `WEBHOOK_SECRET` or `webhook.secret`
- Events: **Pull Request Review Request**, **Pull Request Comments**, and **Pull Request Review Comments**

The corresponding event types are `pull_request_review_request`, `pull_request_comment`, and `pull_request_review_comment`. The first event starts a review only when `action` is `review_requested` and the requested reviewer is the configured bot. `pull_request_sync` is not needed. Gitea signs the raw request body with HMAC-SHA256; Giteye verifies `X-Gitea-Signature` before decoding it.

Create a dedicated bot account and request it as a reviewer on pull requests. For a classic access token, the required scopes are `read:repository`, `write:repository`, `read:issue`, and `write:issue`. The bot must also have access to each repository. Giteye reads pull request metadata, diff, reviews, and discussion comments through the Gitea API. It posts formal reviews, inline comments, thread replies, and—when enabled—a short failure comment.

## Review behavior

- A reviewer request addressed to the bot starts a full review. A re-request starts another review with prior findings and discussion in context. Opening or synchronizing a pull request alone does not trigger a review.
- A comment triggers a response only when its parent chain reaches a Giteye comment. The model sees that thread and the current diff; it can acknowledge an accepted finding, explain a disputed one, or re-check changed code.
- Duplicate webhook deliveries are suppressed in memory. A newer relevant event for the same PR cancels its active review. `max_concurrent_reviews` limits work across all PRs.
- Inline comments are posted only when their file and new-side line map to the current diff. Invalid comments are dropped. If valid inline comments exist, the model decides whether a separate summary adds useful information.
- `notify_on_failure` defaults to `true`; it controls the generic PR note posted after a hard failure. The detailed cause is written to the service log.

## Configuration

Giteye reads `config.yaml` from the working directory by default. If the file does not exist, it starts from defaults and environment variables. If present, YAML is loaded first and supported environment variables override it. Invalid YAML and unknown YAML keys are fatal errors. The webhook secret and the Gitea/AI connection settings are required.

### Settings

| YAML key | Required | Default | Purpose |
| --- | --- | --- | --- |
| `listen_addr` | No | `:8080` | HTTP listen address. |
| `gitea.url` | Yes | — | Gitea base URL, including any URL prefix. |
| `gitea.token` | Yes | — | Bot access token. |
| `gitea.bot_username` | Yes | — | Username used to identify review requests and bot comments. |
| `webhook.secret` | Yes | — | Secret used to verify webhook signatures. |
| `ai.base_url` | Yes | — | OpenAI-compatible API base URL, such as `https://api.openai.com/v1`. |
| `ai.api_key` | Yes | — | API key for the configured AI endpoint. |
| `ai.model` | Yes | — | Model name sent to the Chat Completions API. |
| `mcp.url` | No | Empty (disabled) | Streamable HTTP URL for an optional MCP server. |
| `mcp.token` | No | Empty | Optional token sent to the MCP server. |
| `mcp.auth_header` | No | `Authorization` when a token is set | Header name for `mcp.token`. For `Authorization`, a token without a scheme is sent as `Bearer <token>`. |
| `mcp.allowed_tools` | No | Empty | Exact MCP tool names explicitly allowed even when they are not marked read-only by the server. |
| `log_api_requests` | No | `false` | Log metadata for each AI and MCP request. Prompts, diffs, tool arguments, and results are omitted. |
| `notify_on_failure` | No | `true` | Post a generic PR comment after a hard review failure. |
| `max_concurrent_reviews` | No | `4` | Maximum number of active reviews across all PRs. Values below 1 use the default. |
| `max_tool_rounds` | No | `6` | Maximum number of AI rounds that may request MCP tools. Values below 1 use the default. |
| `review_language` | No | `ru` | Language for review output. |
| `system_prompt_override` | No | Empty | Replace the complete default system prompt. |
| `system_prompt_addition` | No | Empty | Append instructions after the selected system prompt. |
| `repositories` | No | Empty | Per-repository settings keyed by `owner/repo`. |

All byte and MCP discovery limits are under `limits`. Values are bytes unless the key says otherwise. A value below 1 uses the default; limits cannot be disabled with zero.

| YAML key | Default | Purpose |
| --- | ---: | --- |
| `limits.webhook_payload_bytes` | `2097152` (2 MiB) | Maximum signed webhook body size. |
| `limits.review_context_bytes` | `262144` (256 KiB) | Maximum PR context sent to the AI endpoint. |
| `limits.gitea_review_comments_bytes` | `33554432` (32 MiB) | Maximum response size for each inline review-comment API page. |
| `limits.mcp_tool_result_bytes` | `65536` (64 KiB) | Maximum result size forwarded from an MCP tool to the model. |
| `limits.mcp_tool_list_pages` | `32` | Maximum pages read during MCP tool discovery. |
| `limits.mcp_discovered_tools` | `256` | Maximum tools accepted from MCP discovery. |
| `limits.mcp_exposed_tools` | `128` | Maximum tool schemas sent to the model. |
| `limits.mcp_tool_schema_bytes` | `65536` (64 KiB) | Maximum size of one MCP tool schema. |
| `limits.mcp_total_schema_bytes` | `262144` (256 KiB) | Maximum combined size of schemas sent to the model. |

Per-repository entries support `review_language`, `system_prompt_override`, `system_prompt_addition`, and `notify_on_failure`. A repository value overrides the corresponding global setting; omitted values inherit it. Example:

```yaml
repositories:
  example/project:
    review_language: en
    system_prompt_addition: "Prefer concise comments."
    notify_on_failure: false
```

### Environment variables

These variables override the corresponding global YAML setting:

| Variable | YAML key |
| --- | --- |
| `GITEYE_LISTEN_ADDR` | `listen_addr` |
| `GITEA_URL` | `gitea.url` |
| `GITEA_TOKEN` | `gitea.token` |
| `GITEA_BOT_USERNAME` | `gitea.bot_username` |
| `WEBHOOK_SECRET` | `webhook.secret` |
| `AI_BASE_URL` | `ai.base_url` |
| `AI_API_KEY` | `ai.api_key` |
| `AI_MODEL` | `ai.model` |
| `MCP_URL` | `mcp.url` |
| `MCP_TOKEN` | `mcp.token` |
| `MCP_AUTH_HEADER` | `mcp.auth_header` |
| `MCP_ALLOWED_TOOLS` | `mcp.allowed_tools` (comma-separated) |
| `GITEYE_LOG_API_REQUESTS` | `log_api_requests` (`true`/`false`) |
| `REVIEW_LANGUAGE` | `review_language` |

There are no environment overrides for prompts, notification policy, concurrency, tool rounds, or per-repository settings; configure those in YAML. See [config.example.yaml](config.example.yaml) and [.env.example](.env.example) for complete examples.

## Data flow and storage

```text
Gitea webhook ──signed event──> Giteye ──PR data and diff──> Gitea API
                                   │
                                   ├──prompt, diff, history/thread──> configured AI endpoint
                                   ├──discovered tool calls/results──> optional MCP server
                                   └──review, replies, failure note──> Gitea API
```

- Giteye accepts the webhook body in memory, verifies its signature, and ignores unrelated events. It does not store webhook payloads or clone repositories.
- For a full review, Giteye fetches PR metadata, the current diff, prior reviews, inline review comments, and PR discussion. It sends that context with the system prompt to the configured AI endpoint. For a thread reply, it sends the current diff, PR metadata, and active thread.
- The AI endpoint receives PR content. If MCP is configured, Giteye discovers its tools and provides them to the model. Selected tool calls, their arguments, and returned results are part of the AI conversation; the MCP server may also process or forward them according to its implementation.
- Giteye has no database or persistent review cache. Active review context exists in process memory while work runs. Delivery IDs are kept in memory and entries older than 24 hours are pruned when another review event is accepted; a process restart clears the cache.
- Reviews and comments posted by Giteye are stored by Gitea. AI and MCP providers may retain requests or tool data under their own policies. Choose endpoints and provider settings appropriate for the code and discussion data in your repositories.
- Logs go to standard output. They include PR identifiers, event type, duration, tool-call counts, token usage when available, and failure causes. Set `log_api_requests` or `GITEYE_LOG_API_REQUESTS` to `true` to log metadata for each AI and MCP request, including duration, payload sizes, status, and available token usage. Request and response contents, prompts, diffs, tool arguments, and MCP results are not logged. Configured credentials are redacted from error messages.
- The `limits` section controls request and context sizes, MCP discovery, and tool schemas/results. Defaults are shown above.

## MCP

MCP is optional. An empty `mcp.url` disables it; reviews then use webhook and Gitea API context only. When enabled, Giteye connects over Streamable HTTP and discovers tools with `tools/list`. By default, only tools declaring the MCP `readOnlyHint` annotation are offered to the model. Add exact names to `mcp.allowed_tools` only when you trust those tools and intend to allow them; this also opts in tools without a read-only annotation. Tool names and schemas are not hardcoded. MCP connection or discovery failures are logged and the review continues without tools.

Treat the configured MCP endpoint as trusted: tool annotations are supplied by that server and are not a security boundary. Giteye sends tool arguments to the selected MCP server and sends returned results to the AI endpoint. Do not configure an MCP server with write-capable tools unless you explicitly allow them and accept that risk.

## Run from source

```sh
go run ./cmd/giteye
```

The `-config` flag can select a YAML file; it defaults to `config.yaml` and is optional if all required settings are supplied through environment variables.

## Docker

```sh
docker build -t giteye .
docker run --rm -p 8080:8080 --env-file .env giteye
```

To provide YAML configuration to the container, mount it at `/app/config.yaml`:

```sh
docker run --rm -p 8080:8080 --env-file .env \
  -v "$PWD/config.yaml:/app/config.yaml:ro" giteye
```

## Development

```sh
go test ./...
go build ./cmd/giteye
```

## License

Giteye is licensed under the [MIT License](LICENSE).
