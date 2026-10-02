# Prism browser adapter

The optional adapter imports an existing OpenAI OAuth account into an isolated
Prism browser context. It uses the real Prism web application, including its
Sentinel SDK. It does not hold or refresh OAuth refresh tokens. Sub2API remains
the only owner of refresh-token rotation and supplies current access tokens.

The old Free-Astra adapter remains available for manual sessions. The browser
adapter reads the model list Prism itself serves to the current account (the
Statsig `prism_codex_models` config the model menu is built from); it does not
promise Astra availability and never falls back to another model. A model that
Prism does not list for the account (for source 32 on 2026-10-02: `gpt-5.5`,
`gpt-5.6-luna`, `gpt-6-sol`, `gpt-6-astra`) cannot be relayed through Prism; the
same model may still work through the OAuth account itself.

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
| `PRISM_REQUEST_TIMEOUT` | `1800` (30 min) | 30-3600 | Seconds for one bootstrap or generation. Prism returns the whole answer only when it is finished, and long outputs at `xhigh` effort (for example a full HTML animation) can take many minutes. The reverse proxy in front of Sub2API must allow at least as long a read timeout. |
| `PRISM_BODY_LIMIT` | `8388608` (8 MiB) | 4096-33554432 (32 MiB) | Largest request body in bytes. Codex requests with tool schemas and history often exceed 512 KiB. Management requests stay limited to 128 KiB. |
| `PRISM_MAX_TRANSCRIPT_CHARS` | `32000` | 1000-1000000 | Characters of earlier conversation kept per request; older entries are replaced by a note. The final user message is never cut. |
| `PRISM_QUEUE_LIMIT` | `8` | 1-64 | Waiting requests per account. |
| `PRISM_MAX_ACCOUNTS` | `16` | 1-256 | Provisioned source accounts. |
| `PRISM_ACCOUNT_CONCURRENCY` | `2` | 1-4 | Independent workers per source account. Each worker has its own browser context, page and project. |
| `PRISM_MAX_WORKERS` | `32` | 1-1024 | Global browser-context limit, including contexts initializing or closing. |
| `PRISM_ACCOUNT_START_LIMIT` | `0` | 0-120 | Native starts per source across every worker, probe and retry. Zero disables admission pacing. |
| `PRISM_START_WINDOW_SECONDS` | `65` | 1-3600 | Sliding admission window when a start limit is enabled. |
| `PRISM_TRANSIENT_RETRIES` | `1` | 0-1 | Automatic resubmissions of a request that failed because Prism's own servers returned an HTTP 5xx. `0` turns it off. |
| `PRISM_TRANSIENT_RETRY_DELAY_SECONDS` | `4` | 0-60 | Pause before the resubmission, so an overloaded Prism has a moment. |
| `PRISM_TRANSIENT_RETRY_WAIT_SECONDS` | `15` | 0-120 | Longest wait for an idle worker to take the resubmission before the original error is returned. |
| `PRISM_PROMPT_CACHE_TTL_SECONDS` | `600` | 0-3600 | How long a processed prompt counts toward the estimated cache read (see *Usage is estimated*). `0` reports no cached tokens. |

Invalid values stop the adapter at startup.

The image pins Playwright and Chromium to `1.56.1`. Workers share one Chromium
process, while their contexts, pages and projects remain independent. Size
memory for active workers. The primary worker keeps the existing metadata path;
additional workers store metadata under `workers/<slot>/<source>.json`.
`PRISM_MAX_ACCOUNTS` bounds provisioned source metadata and `PRISM_QUEUE_LIMIT`
bounds waiting work per account. Sub2API synchronizes the managed account's
concurrency to the actual ready worker count. A partially available pool can
continue using its healthy workers.

Concurrency is separate from the upstream's rate allowance. Two controlled
production runs on source 32 accepted four native calls, then immediately
refused subsequent calls until a later window. That deployment therefore uses
`PRISM_ACCOUNT_START_LIMIT=4` and `PRISM_START_WINDOW_SECONDS=65`. This is an
observed allowance for that source, not a promise about other Prism accounts.
Admission happens before UI submission and Sentinel generation. A shared FIFO
window includes readiness probes and the existing single resubmission attempt;
it does not add retries. Waiting keeps the ordinary request timeout, SSE
keepalives and disconnect cancellation. Revocation cancels pending admissions
while preserving consumed allowance. No pacing applies by default. Queued calls
can take longer than the upstream generation itself when this limit is enabled.
The window records admission before UI work, with the deployment's five-second
margin covering its measured submission overhead. It does not guarantee the
upstream's timing under an unusually slow UI. Window state is in memory; after
a restart, allow one configured window since the last native call before
starting a load test or admitting another burst.

### Automatic retry of Prism server errors

Prism sometimes answers with an HTTP 5xx (its servers are overloaded or a gateway failed): on the start
request, on a status poll, or as the terminal result of a turn. Its own web app does not retry these, so
without help the request fails after minutes of work. The pool resubmits such a request **once**:

- Only for a 5xx from Prism, never for 4xx, a rate limit (403/429), a rejected model, an expired session,
  an empty answer or any failure of the sidecar itself.
- Only before any text was published to the client. SSE comments and lifecycle frames do not count, so a
  streaming client can see the retry only as a longer wait.
- After a short pause (`PRISM_TRANSIENT_RETRY_DELAY_SECONDS`), on another idle worker of the same
  account (a failed turn usually closes its own browser context), or on the same one if it is still
  usable. If no worker frees up within `PRISM_TRANSIENT_RETRY_WAIT_SECONDS` the original error is returned
  and the gateway can fail over to another account.
- At most one extra attempt; a second failure is returned as it is. The retry shares the request's
  `PRISM_REQUEST_TIMEOUT` and takes another start from the per-source allowance.
- Replaying is safe: Prism returns text only and runs nothing on the caller's machine; tool calls are
  parsed from that text and executed by the client. A failed first turn may still be finishing on Prism's
  side (a stop is requested), so a retry can cost one extra generation there.

With `PRISM_ACCOUNT_CONCURRENCY=1` there is no second worker, so the retry is only used when the same
worker is still usable.

## Management API

All management requests use `Authorization: Bearer <PRISM_MANAGEMENT_KEY>`.
Source IDs are positive decimal Sub2API account IDs.

- `PUT /internal/accounts/:source/session` accepts `access_token`, `api_key`,
  `expires_at` (Unix seconds), and at least one of `expected_email` or
  `expected_user_id`. Each supplied identity field must match the authenticated
  `/auth/session` identity. `expected_user_id` is a user ID, never a workspace
  ID. Provisioning is idempotent. Unchanged credentials refresh expiry without
  waiting for active generations. Credential changes wait for active work, then
  authenticate each worker and reopen previously verified projects without a
  new model probe. Subsequent credentials for that source must resolve to the
  same actual Prism user ID.
- `POST /internal/accounts/:source/bootstrap` accepts `{}`. It creates a
  dedicated blank project through Prism's normal UI, or reopens the saved one,
  waits for sandbox sync and a successful native HTTP heartbeat, and discovers
  models. It waits up to 20 seconds for Prism's feature-flag client (Statsig) to
  finish loading and then takes the catalog from the `prism_codex_models`
  config itself. The model menu is not used for this: its React state can stay
  on a one-model loading fallback (`gpt-5.6-sol`) although the client already
  holds the full list. Only if the client exposes no config is the menu read as
  a fallback, and a menu that collapses to exactly `gpt-5.6-sol` while the saved
  catalog was larger is read once more after 3 seconds. Its first readiness check sends one minimal low-effort request and
  requires the result `READY`. A successful project is not probed again after
  token refresh, restart, or normal background restoration.
- A failed first probe is never replayed in the background. An explicit manual
  reconnect can send `{"retry_probe":true}` to allow one further probe. The
  budget of one bootstrap or generation is `PRISM_REQUEST_TIMEOUT` seconds
  (default 1800, at most 3600).
- `GET /internal/accounts/:source/status` returns `phase`, `ready`, `models`
  and optional `error_code`, `last_heartbeat_at` (Unix seconds), `project_id`.
  It also returns `concurrency`, `pool_size`, `ready_workers` (including busy
  workers), `busy_workers` and `queued`.
  It never returns credentials or identifying email addresses.
- `DELETE /internal/accounts/:source/session` immediately blocks the account key,
  cancels waiting requests and closes all its browser contexts. Managed projects
  and stable identity remain, so
  reauthorization can recover them. It does not delete any Prism project. Sub2API
  calls it when the source OAuth account is temporarily unusable, so the user
  routes then answer `503 account_not_ready` (see below), not 401.

`GET /health` checks service availability only. Use account status to decide
whether a specific account can serve requests. Readiness requires an unexpired
OAuth token, a verified project and a recent successful native heartbeat.
There is no periodic model ping. One specific native terminal error can recover
inside the same user-request timeout: HTTP 403, reason `unknown`, and the exact
Prism message asking to submit the prompt again. The bridge reopens the same
managed project, waits for native readiness, refreshes the model catalog,
then resubmits the unchanged request once if its model is still available.
The first attempt emits no client tool call. Cancellation, revocation, other
errors and readiness probes are never retried this way. A failed second attempt
is returned to the client. It does not take the account offline when it is a per-request
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
replaced with that single message. Official identity, sandbox metadata and
Sentinel proof are preserved. Each call uses a new chat tab; the native start's
`metadata.model` and `reasoning_effort` are set to the requested catalog model
and one of Prism's four efforts (low, medium, high, xhigh), because the UI's own controls can still be on
their loading defaults. The readiness probe uses `gpt-5.6-sol` when the catalog
offers it. Each native
attempt initiates one upstream start. A user call can make a second attempt only
for the narrowly defined terminal resubmission error described above.

For Codex's standard code-mode tools, Sub2API lowers Responses custom tools
(including `exec` in `additional_tools`) to a function with one string argument,
`input`, then restores the reply to `custom_tool_call` and the corresponding
`response.custom_tool_call_input.delta`/`.done` events. HTTP and WebSocket HTTP
bridge requests share this normalization; bridge continuations retain their
tool declarations. Custom tools in the default `functions` namespace use
their public unqualified name (`exec`) in restored replies. The `exec` runtime
description is kept in full so Prism can emit JavaScript using the caller's
documented APIs. The JavaScript runs only in the client executor. No private
model catalog or JavaScript interpreter in the sidecar is required.

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
  actions, followed by the action list (names, parameter schemas, short
  descriptions), the executor environment (`<environment_context>`, `<cwd>`,
  working-directory lines taken from the client's messages), the `TASK` (the last
  user message) and the `TRANSCRIPT` of earlier turns and tool results. The
  client's own system/developer/`instructions` text is forwarded in this mode
  only when it is short (at most 6000 characters combined): then it appears as a
  delimited block after the protocol, with the reminder to still answer in the
  tag format. Huge prompts (Codex, Claude Code) are omitted so that they
  cannot bury the protocol. Codex tools in `mcp__*` namespaces and non-function
  (freeform, web search) tools are not offered.

The reply is parsed back. Each action is a tag,
`<tool_call name="exec">...</tool_call>`, and becomes one Responses `function_call`
item (with its `namespace` unless it is `functions`) or one Chat `tool_calls`
entry with `finish_reason: "tool_calls"`:

- A tool whose schema is a single string (`{"input": string}`, which is how
  Codex's freeform `exec` is lowered) is marked `(raw input)` in the action list.
  Its tag body is the raw text (JavaScript, a patch, a command) passed through
  as `{"input": "<body>"}`; one enclosing code fence and one leading or trailing
  line break are removed. If the model wraps the text in a one-field JSON object
  (`{"code": ...}`, `{"cmd": ...}`, `{"input": ...}`) the wrapper is removed too, so
  `exec` never receives JSON where it expects JavaScript.
- Other tools take one JSON object as the tag body (missing closing braces and
  string-encoded arguments are tolerated).
- A reply may hold up to eight tags. They are returned as several output items
  in order, so the client runs them all and sends one set of results back,
  which saves round trips of 10-30 s each. Text outside the tags (at most 2000
  characters) is returned as a leading progress message.
- A reply with no tag is the final answer (markdown allowed): it becomes the
  text message. The model is told to answer this way when the transcript shows
  the work finished, or when the task needs nothing done on the executor's
  machine (a greeting, a question the conversation already answers), and never
  to claim work the transcript does not show.
- The previous JSON form, `{"tool_call":{"name":...,"arguments":{...}}}` and
  `{"done":"..."}`, is still understood for models that answer in it.
- A call naming a tool that was not offered is dropped; if nothing valid remains,
  the reply is returned as plain text.

Tool results sent back by the client
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
  comes from Chat `reasoning_effort` or Responses `reasoning.effort`. Prism's
  UI offers four levels for every model and all four are passed through:
  `low`, `medium`, `high` and `xhigh` ("Extra High"). `none`/`minimal` run as
  `low` (the lowest Prism has) and the gateway's `max`, Codex's `ultra` and the
  spellings `extrahigh`, `extra-high` and `extra_high` run as `xhigh`; a missing or
  unknown value runs as `medium` and is never an error.
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

Streaming sends Responses `response.created` / `response.in_progress` or the
Chat role chunk as soon as the request is accepted, using one stable response
identity. While Prism generates, the stream sends an SSE comment and (for
Responses) a `response.in_progress` event every 10 seconds.
Codex counts only real events toward its stream idle timeout (default 5 minutes,
`stream_idle_timeout_ms`), so the event, not the comment, is what stops it from
dropping the stream and retrying ("reconnecting 1/5"), which would start a second
generation. Output
events follow when Prism completes; text and tool JSON are validated before
publication. Writes respect client backpressure, and terminal errors use the
appropriate streaming protocol:

- Responses text: `response.created`, `response.in_progress`,
  `response.output_item.added`, `response.content_part.added`, chunked
  `response.output_text.delta`, `response.output_text.done`,
  `response.content_part.done`, `response.output_item.done`, `response.completed`.
- Responses tool calls: for each call in order, `response.output_item.added` (in
  progress, empty arguments), `response.function_call_arguments.delta`/`.done`,
  `response.output_item.done`; then `response.completed`. A progress note is
  emitted first as a text message item.
- Chat: a role chunk, then content chunks and/or one `tool_calls` chunk (one entry
  per call), a final chunk
  with `stop` or `tool_calls`, an optional usage chunk when
  `stream_options.include_usage` is set, and `[DONE]`.

Limitations that remain:

- **No token streaming.** The answer arrives all at once when Prism finishes;
  lifecycle events and SSE comments do not represent generated text.
  Disconnects and timeouts cancel waiting work or invoke the native stop control
  and destroy the active context. A closed worker needs session provisioning
  and bootstrap before reuse; cancelled or timed-out requests
  are never replayed. The specific terminal resubmission error may trigger one
  retry within the original timeout budget.
- **Usage is estimated.** It is a character-based estimate over the flattened
  prompt and the emitted output, marked in `usage.estimation` and
  `X-Prism-Usage: estimated`; it is unsuitable for exact billing.
  Prism reports no token usage at all (its result carries only the text and
  async-job metadata), so cache reads are estimated too, the way OpenAI's
  automatic prompt caching behaves: the longest prefix a prompt shares with a
  prompt the same source account processed within
  `PRISM_PROMPT_CACHE_TTL_SECONDS` counts as `input_tokens_details.cached_tokens`
  (Chat: `prompt_tokens_details.cached_tokens`), once the prompt has 1024
  tokens and in 128-token blocks. `input_tokens` stays the whole prompt and no
  cache writes are reported, matching what OpenAI OAuth accounts report, so the
  gateway bills the cached share at the model's cache-read price. Whether Prism
  actually hit its cache is not observable; a trimmed transcript changes the
  prefix and lowers the estimate.
- **One request at a time per worker.** Requests queue (`PRISM_QUEUE_LIMIT`)
  and a full queue is rejected. Clients on one account can overlap up to its
  ready worker count, while any configured native start allowance is shared.
- **Tool calls are prompt-emulated**, at most eight per reply, run in order (they
  are not parallel). `tool_choice` and strict schemas are not enforced, and weaker
  adherence to the tag protocol shows up as plain text instead of a call.
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
single-start behavior per native attempt, bounded conversation resubmission,
persistent recovery, explicit probe retries, queue limits,
cancellation, HTTP authentication, timeouts and error redaction. They do not
access production credentials or call Prism. Real browser verification remains
necessary after changes to Prism's UI or authentication behavior.
