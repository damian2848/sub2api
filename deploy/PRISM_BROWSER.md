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
| `PRISM_MAX_ATTACHMENTS` | `8` | 1-32 | Attachment parts per request, including history and tool outputs. |
| `PRISM_MAX_ATTACHMENT_BYTES` | `10485760` (10 MiB) | 1-33554432 | Decoded bytes per attachment. Inline base64 also counts toward the HTTP body limit. |
| `PRISM_MAX_TOTAL_ATTACHMENT_BYTES` | `20971520` (20 MiB) | 1-67108864 | Total decoded attachment bytes per request. |
| `PRISM_MAX_TRANSCRIPT_CHARS` | `32000` | 1000-1000000 | Characters of earlier conversation kept per request; older entries are replaced by a note. The final user message is never cut. |
| `PRISM_QUEUE_LIMIT` | `8` | 1-64 | Waiting requests per account. |
| `PRISM_MAX_ACCOUNTS` | `16` | 1-256 | Provisioned source accounts. |
| `PRISM_ACCOUNT_CONCURRENCY` | `2` | 1-4 | Independent workers per source account. Each worker has its own browser context, page and project. |
| `PRISM_MAX_WORKERS` | `32` | 1-1024 | Global browser-context limit, including contexts initializing or closing. |
| `PRISM_ACCOUNT_START_LIMIT` | `0` | 0-120 | Legacy token-bucket capacity when `PRISM_START_BURST` is unset. Zero disables pacing, not rejection cooldowns. |
| `PRISM_START_WINDOW_SECONDS` | `65` | 1-3600 | Legacy refill derivation: window / start limit seconds per token. |
| `PRISM_START_BURST` | legacy start limit | 0-120 | Token capacity per source across every worker, readiness probe and retry; 0 disables pacing. |
| `PRISM_START_REFILL_SECONDS` | legacy window / burst | 0.001-3600 | Initial seconds to replenish one token. |
| `PRISM_START_REFILL_MIN_SECONDS` | initial refill | 0.001-3600 | Lower bound for the adaptive refill interval. |
| `PRISM_START_REFILL_MAX_SECONDS` | max(90, initial refill) | 0.001-3600 | Upper bound in seconds, at least the initial refill and minimum. The derived default also accepts legacy intervals above 90 seconds. |
| `PRISM_START_MAX_WAIT_SECONDS` | max(15, initial refill); 15 without a start limit | 0-3600 | Seconds to wait before failing fast only when another account is available. By default the first request after a depleted bucket can wait for one token. Without a start limit the only wait is a post-rejection cooldown, which is longer than 15 s, so requests are handed to another account at once. |
| `PRISM_TRANSIENT_RETRIES` | `1` | 0-1 | Automatic resubmissions of a request that failed because Prism's own servers returned an HTTP 5xx. `0` turns it off. |
| `PRISM_TRANSIENT_RETRY_DELAY_SECONDS` | `4` | 0-60 | Pause before the resubmission, so an overloaded Prism has a moment. |
| `PRISM_TRANSIENT_RETRY_WAIT_SECONDS` | `15` | 0-120 | Longest wait for an idle worker to take the resubmission before the original error is returned. |
| `PRISM_STATUS_POLL_MS` | `1000` | 0 or 250-10000 | After the page's first successful poll, poll independently at this interval. Failures back off to at most 8000 ms; only three consecutive failures stop independent polling. `0` leaves polling to the page. |
| `PRISM_START_COOLDOWN_SECONDS` | `60` | 0-600 | Minimum pause after a refused start. Admission also waits for the next token. `0` disables this minimum, not token pacing. |
| `PRISM_PREWARM_CHAT` | `true` | `true` / `false` | After a clean turn the worker opens the next chat tab, closes the older chat tabs and waits for the composer while idle, so the next request only fills it in. Each request used to open a tab that was never closed, so the page grew and preparing a request took 5-9 s after ~16 turns. `false` opens the chat per request. |
| `PRISM_STREAM_REASONING` | `true` | `true` / `false` | Forward Prism's reasoning summaries and tool progress (see *Progress while Prism works*) to streaming Responses clients. `false` keeps the stream to heartbeats until the answer. |
| `PRISM_PROMPT_CACHE_TTL_SECONDS` | `600` | 0-3600 | How long a processed prompt counts toward the estimated cache read (see *Usage is estimated*). `0` reports no cached tokens. |

Invalid values stop the adapter at startup.
Leave `PRISM_START_BURST`, `PRISM_START_REFILL_SECONDS`,
`PRISM_START_REFILL_MIN_SECONDS`, `PRISM_START_REFILL_MAX_SECONDS` and
`PRISM_START_MAX_WAIT_SECONDS` blank in the deployment files to inherit their
derived defaults. Hard-coding zero for the new burst would override an existing
nonzero legacy start limit. Refill settings accept fractional seconds; the
initial interval must lie between the minimum and maximum when pacing is on.

The image pins Playwright and Chromium to `1.56.1`. Workers share one Chromium
process, while their contexts, pages and projects remain independent. Size
memory for active workers. The primary worker keeps the existing metadata path;
additional workers store metadata under `workers/<slot>/<source>.json`.
`PRISM_MAX_ACCOUNTS` bounds provisioned source metadata and `PRISM_QUEUE_LIMIT`
bounds waiting work per account. Sub2API synchronizes the managed account's
concurrency to the actual ready worker count. A partially available pool can
continue using its healthy workers.

Concurrency is separate from upstream start pacing. Each source has one shared
FIFO token bucket, including readiness probes and all retries. Legacy settings
`PRISM_ACCOUNT_START_LIMIT=4` and `PRISM_START_WINDOW_SECONDS=65` derive a burst
of 4 and an initial refill of 16.25 seconds per token; explicit new settings
override these defaults. This is not a promise about Prism's actual allowance.
After a refused start the bucket clears its tokens and multiplies the refill
interval by 1.5, up to the configured maximum. Every four accepted starts with
no intervening refusal multiply it by 0.9, down to the minimum. This AIMD-style
learning lives only in memory and resets at restart.

The gateway overwrites `X-Prism-Failover` on managed Prism upstream requests.
`available` means another schedulable, model-compatible account remains outside
the failed-account set (it need not be Prism); `none` means no such alternative
was found. Missing or unknown headers retain the old `available` behavior.
Admission waits exceeding `PRISM_START_MAX_WAIT_SECONDS` return JSON 429 with
the actual retry delay only for `available`. A refused start also returns 429
immediately in that mode and cancels its available waiters. With `none`, waiters
remain queued through cooldown and a refused start retries once on the same
source after both cooldown and token replenishment. A second refusal returns
429. All waiting remains subject to the ordinary request timeout and disconnect
cancellation; no response bytes or SSE keepalives are sent before acceptance.
Revocation cancels admissions without refunding consumed tokens. Account status
includes `start_bucket` (tokens, refill seconds and cooldown milliseconds).
Audits retain `native_start_wait` and `start_rejected_cooldown` and add
`start_bucket_rejected` and `start_bucket_adjusted`.

Independent status polling tolerates transient HTTP errors, unreadable JSON and
request exceptions. After each failure its next interval is
`min(8000, PRISM_STATUS_POLL_MS * 2^consecutive_failures)` milliseconds; a
successful poll resets that streak. Only three consecutive failures stop our
poller and leave completion to the page. Page polling errors remain nonfatal
while our poller is healthy. `upstream_result.own_poll_errors` counts all failed
independent polls in the turn, including failures before a recovered streak.

### Progress while Prism works

Prism returns the answer text only when a turn is finished; while it works, its status poll carries
`codex_live_progress` with short reasoning summaries and the tool calls of Prism's own agent. For a
streaming `/v1/responses` request the sidecar forwards both as one `reasoning` output item (output 0),
in the lifecycle Codex needs to render streaming reasoning: `output_item.added`,
`reasoning_summary_part.added`, `reasoning_summary_text.delta`/`.done`, `reasoning_summary_part.done`, then
`output_item.done` before the answer's own items, which move to output 1 and later. The completed
response lists the reasoning item first. Each distinct summary is sent once (as a part of its own);
tool progress becomes a bold one-line part per kind ("Explored 2 locations", "Searched", "Read 1 file",
"Editing files", "Running a command"), as the Prism page shows it. A part is at most 4000 characters and
a request forwards at most 64 parts. Chat Completions and non-streaming requests are unchanged, and the
audit counts the forwarded parts (`reasoning_forwarded`) without logging any text.

Two consequences. The summaries are real client output, so a stream that has shown one cannot move to
another account if Prism fails later; the client receives the failure and retries itself. And
`first_token_ms` for such requests is the first summary, not the first character of the answer.
The reasoning item has no `encrypted_content`; the sidecar ignores `reasoning` input items, and the
gateway already replays them without their id for OAuth accounts.

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
Model listings declare `input_modalities: ["text", "image"]` and
`output_modalities: ["text"]`; documents use the attachment workflow below.

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
attempt initiates one upstream start. The existing project-refresh retry for
the narrowly defined terminal resubmission error after acceptance is unchanged.
Separately, a start refused before acceptance may be retried once after pacing
and cooldown when no alternative account is available.

**Start rejections.** When Prism answers the start itself with that error ("Please
submit prompt again", HTTP 403 inside the payload), the account is over Prism's
start allowance, not in a broken project state. The sidecar does not refresh the
project. It clears the source's tokens, adapts the refill interval and observes
the minimum `PRISM_START_COOLDOWN_SECONDS` pause. With an alternative account it
answers HTTP 429 with `Retry-After` and an OpenAI rate-limit body
(`type: rate_limit_exceeded`, `resets_in_seconds`), cancels available waiters,
and lets the gateway cool the account and switch. Without an alternative, it
waits for both cooldown and a new token and retries once on the same source;
only a second refusal returns 429. New available requests with short enough
waits may queue; none requests remain queued regardless of the fast-fail
threshold. Measured on 2026-10-02, one source accepted a burst of about 3-4 starts
and then about one more per minute; adaptive pacing does not hard-code that fit.

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

For Responses callers whose system/developer instructions contain
`clickable markdown link` (case-insensitive), the last `<cwd>...</cwd>` inside
an `<environment_context>` block in a user or developer message enables a
deterministic file-link fallback if it is an absolute POSIX path. Assistant
messages, tool outputs, system-message environments and cwd tags outside those
blocks cannot select or override the base directory. Only a final-answer
message without tool calls is rewritten: eligible
inline code such as `src/app.py:12:3` becomes
`[app.py](/absolute/cwd/src/app.py:12:3)`. Existing links and fenced code are
preserved, as are commentary, Chat replies, URLs, glob/tilde paths and bare
filenames such as `account.go:914`. Relative paths are normalized against cwd;
absolute paths are retained. The forwarded `<output_conventions>` remain in
place. The fallback cannot create a link if the model omits a file path or does
not put it in inline code. These locally added links do not inflate estimated
upstream output-token usage.

Accepted input:

- Responses: `input` as string, object or array; `instructions`; items `message`
  (roles `user`, `assistant`, `system`, `developer`), `function_call`,
  `function_call_output`, `custom_tool_call`, `custom_tool_call_output` and
  `additional_tools`. `reasoning` items and server tool traces are skipped.
- Chat: roles `system`, `developer`, `user`, `assistant` (including `tool_calls`)
  and `tool`.
- Content parts `text`, `input_text`, `output_text` or plain strings. Codex
  scaffolding user messages (plugin and skill lists) are dropped.
- Images: Responses `input_image.image_url` and Chat `image_url.url`, as a
  base64 data URL or a public HTTP(S) URL. PNG, JPEG, WebP and GIF are accepted.
- Files: Responses `input_file` with `filename` and `file_data` or `file_url`,
  and Chat `file` parts with the same fields inside their `file` object. `file_data` accepts base64 or a
  base64 data URL. PDF and UTF-8 text files are accepted, including Markdown,
  CSV, JSON and source-code text. Anthropic images and base64/URL documents
  are converted by Sub2API before forwarding. OpenAI `file_id` references
  cannot be resolved by this adapter.
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

### Attachment upload and references

Attachments are decoded or downloaded before browser admission. Downloads have
a 15-second deadline per file, at most three redirects and decoded byte limits.
Only public HTTP(S) destinations are allowed; credentials, private or reserved
addresses and compressed responses are refused. Each DNS result and redirect
is validated, and the connection uses the validated address. No caller API key
or OAuth token is sent to attachment URLs.

Each attachment gets a sanitized filename containing its content digest. The
worker uploads the bytes through Prism's official **Upload files & photos**
control and waits for the composer to register the file and finish pending
uploads. This matters: forwarding an OpenAI data URL, or only POSTing file
bytes, does not register a readable Prism project attachment.

The adapter validates the native `input_file` reference against the managed
project and expected filename, then adds its `/prism-uploads/...` path to the
flattened message. `[Attachment N]` markers preserve where files appeared in
history and tool outputs. The prompt permits native read-only inspection of
these files, while caller tool calls still use the emulated action protocol.
Unrelated native history is never imported into the API conversation. Every
retry registers the same bytes in its selected worker's project.

Uploads remain in the worker's dedicated managed project; the same filename
and content can use Prism's existing-file control. They are not deleted after
each turn. Request limits bound each upload, not the lifetime size of a project.
Failed or cancelled preparation closes the browser context before reuse.
No SSE headers are sent until Prism accepts the subsequent model start.

Any reverse proxy or CDN in front of the sidecar must allow a response-header
wait longer than the longest queue/cooldown/retry delay. No response headers,
body bytes or heartbeat frames are sent while admission is pending, so a
pre-response timeout (for example Cloudflare's default 100-second no-response
limit) can interrupt a healthy queued request. Size that timeout for the total
`PRISM_REQUEST_TIMEOUT` budget when no alternative account exists, or bypass
the short-timeout proxy on this internal hop. Sub2API's
`GATEWAY_OPENAI_RESPONSE_HEADER_TIMEOUT` should be `0` (unlimited), or exceed
the longest pre-acceptance wait. SSE keepalives only help after acceptance.

Streaming sends Responses `response.created` / `response.in_progress` or the
Chat role chunk as soon as Prism has accepted the start, using one stable response
identity. Until then (queueing, the UI steps, the start request) nothing is sent,
so a refused start can still be answered with a plain 429 the gateway fails over. While Prism generates, the stream sends an SSE comment and (for
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
  prompt and the model output before local link rewriting, marked in `usage.estimation` and
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
  prefix and lowers the estimate. Attachment requests report no estimated
  cached tokens; image/PDF inspection and file bytes are not included in the
  character-based token estimate.
  The estimator stores at most 64 prompts per source, as packed 16-byte chained
  SHA-256 hashes of 384 JavaScript characters per block plus length and last-use
  metadata, not plaintext. UTF-16 code-unit hashing preserves prefix identity
  even across surrogate boundaries. Only complete matching blocks count toward
  cached tokens (128 per block, at least eight blocks); a final partial hash is
  retained only to distinguish entries. A 256 KiB ASCII prompt uses 10,928 bytes
  of hashes, about 11 KiB, instead of retaining its full text.
- **One request at a time per worker.** Requests queue (`PRISM_QUEUE_LIMIT`)
  and a full queue is rejected. Clients on one account can overlap up to its
  ready worker count, while any configured native start allowance is shared.
- **Tool calls are prompt-emulated**, at most eight per reply, run in order (they
  are not parallel). `tool_choice` and strict schemas are not enforced, and weaker
  adherence to the tag protocol shows up as plain text instead of a call.
- **Audio and OpenAI file IDs are unsupported.** Audio parts still return
  `400 image_input_not_supported` (the legacy error code); unresolved `file_id`
  returns `400 attachment_file_id_not_supported`. A native WAV upload was
  accepted, but the model could not read it in the controlled test. The adapter
  also rejects a `previous_response_id`
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
  the group, without penalising the Prism account: requests with
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
attachment decoding, bounded public-URL downloads, native upload registration
and exact file references,
single-start behavior per native attempt, bounded conversation resubmission,
persistent recovery, explicit probe retries, queue limits,
cancellation, HTTP authentication, timeouts and error redaction. They do not
access production credentials or call Prism. Real browser verification remains
necessary after changes to Prism's UI or authentication behavior.
