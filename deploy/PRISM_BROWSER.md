# Prism browser adapter

The optional adapter imports an existing OpenAI OAuth account into an isolated
Prism browser context. It uses the real Prism web application, including its
Sentinel SDK. It does not hold or refresh OAuth refresh tokens. Sub2API remains
the only owner of refresh-token rotation and supplies current access tokens.

The old Free-Astra adapter remains available for manual sessions. The browser
adapter discovers the current account's actual model menu; it does not promise
Astra availability and never falls back to another model.

## Deployment

Set `PRISM_MANAGEMENT_KEY` to an independent random secret of at least 32
characters, for example the output of `openssl rand -hex 32`. Configure the same
secret on Sub2API. Keep it out of Git and public client configuration.

Use the optional compose file alongside the main compose deployment:

```sh
docker compose --env-file .env -f docker-compose.yml -f docker-compose.prism-browser.yml up -d --build prism-browser
```

Set Sub2API's `PRISM_BROWSER_BASE_URL=http://prism-browser:8319`. For an existing
Sub2API container using host networking, use `http://127.0.0.1:8319` and retain
`PRISM_BROWSER_BIND=127.0.0.1:8319`. Do not publish this port on a public address.
The compose service has no dependency that delays or restarts Sub2API.

The data directory contains atomic, mode-600 JSON metadata; its mode is 700.
It holds source IDs, managed project IDs, current model catalogs, hashes of
per-account API keys and stable Prism user identities, and readiness-probe
state. Access tokens, refresh tokens and cookies are never saved there. After
an adapter restart, Sub2API must provision a fresh access token and bootstrap
the saved project again. Browser contexts and cookies otherwise remain only
in memory. Existing Prism projects are not imported or modified.

Tunables (environment variables of the adapter, all forwarded by the optional
compose file and listed in `.env.prism-browser.example`):

| Variable | Default | Allowed | Meaning |
| --- | --- | --- | --- |
| `PRISM_REQUEST_TIMEOUT` | `240` | 30-600 | Seconds for one bootstrap or generation. |
| `PRISM_BODY_LIMIT` | `8388608` (8 MiB) | 4096-33554432 (32 MiB) | Largest request body in bytes. Codex requests with tool schemas and history often exceed 512 KiB. Management requests stay limited to 128 KiB. |
| `PRISM_MAX_TRANSCRIPT_CHARS` | `32000` | 1000-1000000 | Characters of earlier conversation kept per request; older entries are replaced by a note. The final user message is never cut. |
| `PRISM_QUEUE_LIMIT` | `8` | 1-64 | Waiting requests per account. |
| `PRISM_MAX_ACCOUNTS` | `16` | 1-256 | Provisioned source accounts. |

Invalid values stop the adapter at startup.

The image pins Playwright and Chromium to `1.56.1`. Each active source account
uses a separate browser context and page, so size memory for the number of
active accounts. `PRISM_MAX_ACCOUNTS` bounds provisioned source metadata and
`PRISM_QUEUE_LIMIT` bounds waiting work per account.

## Management API

All management requests use `Authorization: Bearer <PRISM_MANAGEMENT_KEY>`.
Source IDs are positive decimal Sub2API account IDs.

- `PUT /internal/accounts/:source/session` accepts `access_token`, `api_key`,
  `expires_at` (Unix seconds), and at least one of `expected_email` or
  `expected_user_id`. Each supplied identity field must match the authenticated
  `/auth/session` identity. `expected_user_id` is a user ID, never a workspace
  ID. Provisioning is idempotent and never calls a model. Subsequent credentials
  for that source must resolve to the same actual Prism user ID.
- `POST /internal/accounts/:source/bootstrap` accepts `{}`. It creates a
  dedicated blank project through Prism's normal UI, or reopens the saved one,
  waits for sandbox sync and a successful native HTTP heartbeat, and discovers
  models. Before opening the model menu it waits up to 20 seconds for Prism's
  feature-flag client (Statsig) to finish loading, because the menu shows a
  fallback list with a single model until then. If the menu still collapses to
  exactly `gpt-5.6-sol` while the saved catalog was larger, it is read once more
  after 3 seconds and that second result is accepted. Its first readiness check sends one minimal low-effort request and
  requires the result `READY`. A successful project is not probed again after
  token refresh, restart, or normal background restoration.
- A failed first probe is never replayed in the background. An explicit manual
  reconnect can send `{"retry_probe":true}` to allow one further probe. The
  budget of one bootstrap or generation is `PRISM_REQUEST_TIMEOUT` seconds
  (default 240, at most 600).
- `GET /internal/accounts/:source/status` returns `phase`, `ready`, `models`
  and optional `error_code`, `last_heartbeat_at` (Unix seconds), `project_id`.
  It never returns credentials or identifying email addresses.
- `DELETE /internal/accounts/:source/session` stops its browser context and
  revokes the account API key. Managed project and stable identity remain, so
  reauthorization can recover them. It does not delete any Prism project. Sub2API
  calls it when the source OAuth account is temporarily unusable, so the user
  routes then answer `503 account_not_ready` (see below), not 401.

`GET /health` checks service availability only. Use account status to decide
whether a specific account can serve requests. Readiness requires an unexpired
OAuth token, a verified project and a recent successful native heartbeat.
There is no periodic model ping. A failed model request is returned once and
never replayed. It does not take the account offline when it is a per-request
failure (`prism_generation_failed`, `prism_empty_output`, `prism_invalid_output`,
`upstream_model_mismatch`, `model_not_available`, `request_cancelled`, timeouts,
validation errors) and the browser session is still alive. Only a dead browser
page or a session-level failure (`session_expired`, `browser_session_closed`,
`session_closed`, `browser_ui_*_failed`, a project mismatch) marks the account
not ready for explicit/background bootstrap recovery.

## OpenAI API

Per-account clients use `Authorization: Bearer <api_key>` and the base URL
`/accounts/:source/v1`. The only routes are `models`, `responses`, and
`chat/completions`. The key is persisted as a SHA-256 hash and cannot authorize
another source account or a management operation.

Authentication on these routes is deliberately not a 401 unless the key itself is
wrong, because Sub2API treats an upstream 401 on an API-key account as a dead
credential and disables the account permanently. An unknown source (for example
a fresh data directory), a source without a provisioned key, and a revoked or
not yet reauthorized session all return `503 account_not_ready`, so the gateway
fails over and cools the account down only temporarily. `401 invalid_api_key` is
returned only when a key hash exists and the presented key does not match it.
Management routes are unchanged (`401 invalid_management_key`, `404` for an
unknown source).

A Prism account is meant to behave, from the client's point of view, like an
OpenAI relay account: Codex CLI, Claude Code (Sub2API converts Anthropic requests
to Responses or Chat before they reach this adapter) and generic chat clients can
use it, including multi-turn conversations, instructions and tool calling.
Prism itself cannot do this natively. Live tests showed that it reads only the
text of the **last user message** (system/developer roles and earlier items in
`input` are ignored), runs its own sandbox tools instead of the caller's tools,
does not stream tokens and does not report usage. The adapter therefore folds
the whole request into the text of one user message and emulates the rest. The
approach follows the free-astra project (MIT; see
`tools/prism-browser/THIRD_PARTY_NOTICES.md`).
A normal UI send produces the SDK's authenticated request; only its input is
replaced with that single message. Official identity, sandbox metadata, model
selection and Sentinel proof are preserved. Each call uses a new chat tab, the
requested catalog model and a low/medium/high reasoning effort. One user call can
initiate exactly one upstream start.

How a request is turned into the single user message:

- **Plain prompt.** A lone user text message without instructions or tools is
  passed through unchanged, byte for byte.
- **Instructions or history, no tools.** The text starts with a short preamble
  (answer directly, do not touch files or use Prism's own tools), then the
  `system`/`developer` messages or Responses `instructions` in a delimited block,
  then `Conversation so far. Respond to the FINAL user message.` followed by
  `[user]` / `[assistant]` entries.
- **Tools present** (Responses `tools` of type `function`, Chat `tools`, or the
  Codex `additional_tools` input item). The text uses a next-action-emitter
  framing: the model is told it is one component of a pipeline that only emits
  one JSON action, followed by the action list (names, parameter schemas, short
  descriptions), the executor environment (`<environment_context>`, `<cwd>`,
  working-directory lines taken from the client's messages), the `TASK` (the last
  user message) and the `TRANSCRIPT` of earlier turns and tool results. The
  client's own system/developer/`instructions` text is forwarded in this mode
  only when it is short (at most 6000 characters combined): then it appears as a
  delimited block after the protocol, with the reminder to still answer in the
  JSON action format. Huge prompts (Codex, Claude Code) are omitted so that they
  cannot bury the protocol. Codex tools in `mcp__*` namespaces and non-function
  (freeform, web search) tools are not offered.

The reply is parsed back: `{"tool_call":{"name":...,"arguments":{...}}}` becomes a
Responses `function_call` item (with its `namespace` unless it is `functions`) or
a Chat `tool_calls` entry with `finish_reason: "tool_calls"`. Code fences, short
leading prose, missing closing braces and string-encoded arguments are tolerated.
`{"done":"..."}` carries the complete final reply (multi-line, markdown allowed,
newlines escaped inside the JSON string; raw newlines are tolerated). The model is
told to use it when the transcript shows the work finished, or when the task needs
nothing done on the executor's machine (a greeting, a question answerable from the
conversation), and never to claim work the transcript does not show. It becomes
the text message. A reply naming a tool that was not offered, or not JSON at all,
is returned as plain text. Tool results sent back by the client
(`function_call_output`, `custom_tool_call_output`, Chat `tool` messages) are
placed in the transcript, so the loop continues turn by turn.

Accepted input:

- Responses: `input` as string, object or array; `instructions`; items `message`
  (roles `user`, `assistant`, `system`, `developer`), `function_call`,
  `function_call_output`, `custom_tool_call`, `custom_tool_call_output` and
  `additional_tools`. `reasoning` items and server tool traces are skipped.
- Chat: roles `system`, `developer`, `user`, `assistant` (including `tool_calls`)
  and `tool`.
- Content parts `text`, `input_text`, `output_text` or plain strings. Codex
  scaffolding user messages (plugin and skill lists) are dropped.
- `model` must be in the account catalog (`model_not_available`). Reasoning effort
  comes from Chat `reasoning_effort` or Responses `reasoning.effort`:
  `none`/`minimal`/`low` run as `low`, `medium` as `medium`, `high`/`xhigh` as
  `high`; a missing or unknown value runs as `medium` and is never an error.
- All other parameters (`temperature`, `top_p`, `tool_choice`,
  `parallel_tool_calls`, `include`, `prompt_cache_key`, `text`, `store`,
  `metadata`, `user`, `service_tier`, `max_output_tokens`, `truncation`, ...) are
  ignored, not rejected.

Size handling: the transcript (everything except the final user message) is
clamped to `PRISM_MAX_TRANSCRIPT_CHARS` characters (default 32000), keeping the
newest entries and noting how many earlier steps were omitted; single tool results
are shortened to their head and tail. Long histories are therefore never
rejected. A request fails with `input_too_large` only if the final flattened
text still exceeds 256 KiB, for example one gigantic final user message or a
very large system prompt without tools. `PRISM_BODY_LIMIT` bounds the HTTP body
(default 8 MiB, at most 32 MiB; management requests stay at 128 KiB).

Streaming keeps the connection alive with SSE comments, then sends the events
for the completed output:

- Responses text: `response.created`, `response.in_progress`,
  `response.output_item.added`, `response.content_part.added`, chunked
  `response.output_text.delta`, `response.output_text.done`,
  `response.content_part.done`, `response.output_item.done`, `response.completed`.
- Responses tool call: `response.output_item.added` (in progress, empty
  arguments), `response.function_call_arguments.delta`/`.done`,
  `response.output_item.done`, `response.completed`.
- Chat: a role chunk, then content chunks or one `tool_calls` chunk, a final chunk
  with `stop` or `tool_calls`, an optional usage chunk when
  `stream_options.include_usage` is set, and `[DONE]`.

Limitations that remain:

- **No token streaming.** The answer arrives all at once when Prism finishes;
  SSE comments only keep the connection open. Disconnects and timeouts cancel
  queued work or invoke the native stop control, and requests are never replayed.
- **Usage is estimated.** It is a character-based estimate over the flattened
  prompt and the emitted output, marked in `usage.estimation` and
  `X-Prism-Usage: estimated`; it is unsuitable for exact billing.
- **One request at a time per account.** Requests queue (`PRISM_QUEUE_LIMIT`)
  and a full queue is rejected. Several clients on one account serialise.
- **Tool calls are prompt-emulated**, at most one call per turn. Parallel tool
  calls, `tool_choice` and strict schemas are not enforced, and weaker adherence
  to the JSON protocol shows up as plain text instead of a call.
- **Images and files are unsupported.** The sidecar itself rejects `input_image`,
  `image_url`, `input_file`, `input_audio` and `file` parts with
  `400 image_input_not_supported`. It also rejects a `previous_response_id`
  (`400 previous_response_not_supported`): nothing is stored server side, so a
  client must resend the history. These are safety nets; the gateway is meant to
  deal with such requests before they reach a Prism account (next section).
- **Prism's own system prompt and model allowlist apply.** Instructions reach
  the model only as text inside the user message, so they are advisory rather
  than a real system prompt. Only models in the account's discovered catalog can
  be used, and Prism changes that list without notice.
- The OpenAI-API surface is limited to `models`, `responses` and
  `chat/completions`. Errors never contain raw upstream bodies, browser
  exceptions, cookies or access tokens.

## Gateway behavior and Codex WebSocket

The sidecar speaks plain HTTP only, with no WebSocket endpoint. Codex CLI prefers
the Responses WebSocket transport, and the Sub2API gateway serves it for Prism
accounts through its `http_bridge` WebSocket mode:

- Prism accounts are created with
  `openai_apikey_responses_websockets_v2_mode=http_bridge`. Existing Prism accounts
  are self-healed to that mode by the background sync.
- Every WebSocket turn is sent to the sidecar as an ordinary HTTP
  `/accounts/:source/v1/responses` SSE request that carries the full history, with
  `previous_response_id` stripped, because Prism keeps no server-side
  conversation.
- Requests that a Prism account cannot serve are failed over to another account in
  the group, without penalising the Prism account: requests with image, file or
  audio parts, a `previous_response_id` without history to resend, and
  `/responses/compact`. Only when no other account can serve such a request does
  the client get `400` (or WebSocket close code `1008`).
- A Prism account whose browser session is down (not ready, revoked, expired or
  closed) fails over like any other unavailable account (the sidecar answers
  `503 account_not_ready`) and is cooled down only temporarily.

## Verification

```sh
cd tools/prism-browser
npm ci --ignore-scripts
npm test
```

The tests use simulated browser sessions and cover request flattening for
Codex, Claude Code style and plain chat requests, tool-call parsing and output
shapes, ignored and rejected parameters, size clamping, every SSE variant, model
catalog recovery, account health after failed requests, credential isolation,
single-start behavior, persistent recovery, explicit probe retries, queue limits,
cancellation, HTTP authentication, timeouts and error redaction. They do not
access production credentials or call Prism. Real browser verification remains
necessary after changes to Prism's UI or authentication behavior.
