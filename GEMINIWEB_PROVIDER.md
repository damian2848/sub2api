# Gemini Web provider

Sub2API now has a native Gemini Web account type. It uses the normal Gemini
account scheduler, proxy selection, sticky sessions, model mappings, usage
accounting, and failover path. The upstream request is made with a browser
Cookie header instead of an AI Studio API key or OAuth token.

## Account configuration

Create an account with:

```text
platform: gemini
type:     gemini_web
```

The credentials object contains:

```json
{
  "gemini_web_cookie": "__Secure-1PSID=...; __Secure-1PSIDTS=...; ...",
  "auth_user": 0,
  "user_agent": "Mozilla/5.0 ...",
  "model_mapping": {
    "gemini-pro": "gemini-3.1-pro"
  }
}
```

`gemini_web_cookie` must be the complete Cookie value copied from a logged-in
Gemini Web browser session and must include `__Secure-1PSID`. A pasted
`Cookie: ...` header is accepted. `auth_user` is only needed for accounts that
use a non-default Google profile. `user_agent` is optional.

The account can be placed in an ordinary Gemini group. Its concurrency,
priority, proxy, expiry, and model mapping are managed through the same
Sub2API controls as other provider accounts. A rejected Cookie is temporarily
removed from scheduling; a Web rate limit records a short account cooldown.

## Model restrictions and discovery

Gemini Web does not expose the AI Studio `/v1beta/models` endpoint for browser
Cookies. The account model selector and the account-level “sync upstream
models” action therefore use Sub2API's curated Gemini Web catalog. The current
catalog contains the supported Web modes (`gemini-4-argon`, `gemini-3.8-flash`,
`gemini-3.8-flash-thinking`, `gemini-3.1-pro`, `gemini-auto`,
`gemini-3.8-flash-thinking-lite`, `gemini-3.5-flash-lite`, and
`gemini-3.6-flash`). A Cookie cannot reliably reveal which subscription tier a
Google account can use without sending a generation request, so syncing does
not probe or modify the account's quota.

The normal account model whitelist and model mapping still apply. An empty
restriction allows the provider's supported names to be selected at request
time; an explicit whitelist or mapping limits scheduling before the browser
session is called. Unknown Web model IDs are rejected by the provider instead
of being silently sent to an unrelated Gemini API endpoint.

Google may serve a different model even when generation succeeds. The provider
reads the model ID declared in `StreamGenerate` payload index 39, maps known
IDs to catalog names, and passes it to the normal upstream-response model audit.
The request and client-facing model alias remain separate from this observation.
Missing declarations stay unknown; conflicting declarations retain the latest
model and set the existing conflict flag. A mismatch alone does not trigger
failover. Billing follows the channel's existing model-source setting, including
the normal safeguards when `billing_model_source = response_model` is enabled.

The account whitelist's **Test models** action requests strict model validation
(`verify_model: true`). A declared downgrade fails the selected model; missing
or conflicting declarations are shown as unverified and are retained by
**Clear unavailable models**. Unsupported Web names are rejected before sending
a generation request. This stricter test verdict does not change normal gateway
forwarding or background connectivity checks. Antigravity model tests use the
same verdict, comparing the mapped request with the returned `modelVersion`.

## Supported gateway routes

The provider translates the browser response into the existing gateway
contracts for:

- Gemini native `generateContent`, `streamGenerateContent`, and `countTokens`;
- OpenAI-compatible `/v1/chat/completions`;
- Anthropic-compatible `/v1/messages`.

Streaming requests forward browser frames as they arrive on all three
protocols. Claude Messages and Chat Completions reuse the native Gemini stream
converters of regular Gemini accounts, so content blocks, `tool_use` /
`tool_calls` and stop reasons match them. When tools are declared, answer text
is held until the reply is complete, on all three protocols. If no content has
arrived after 5 seconds, the response is committed and the client receives
keepalives (SSE comment pings for Claude Messages and Chat Completions, blank
lines for native Gemini streams). Keepalives are not content, so until the
first content is sent a failed request can still fail over to another account
on the same response. Gemini Web often spends several seconds before its first
frame, which no gateway setting can shorten.

## Canned failure replies

When generation fails, Gemini Web may replace the answer with a fixed message
such as "I seem to be encountering an error. Can I try something else for
you?" and still return HTTP 200. Google marks such replies with
`payload[31] = true` in the `StreamGenerate` frame, a flag that real answers
and policy refusals do not carry. A reply with that flag, or one that is
entirely a known canned phrase (ignoring case, extra whitespace and curly
quotes), fails the request and is not billed: it fails over to another account
while the client has received nothing but keepalives, and otherwise ends with
an error saying the request was not billed. Streamed text that carries the
flag, or could still become a canned phrase, is held back.

An account that returns three canned replies within ten minutes is taken out
of scheduling for ten minutes (`gemini_web.canned_reply_cooldown`), so sticky
sessions stop landing on a degraded account. A single canned reply only
switches accounts.

Built-in phrases are always recognized, including "Sorry, something went
wrong. Please try your request again." Administrators can add more under Settings →
Gateway → "Gemini Web canned error replies" (`GET`/`PUT
/api/v1/admin/accounts/gemini-web/canned-replies`, stored in the
`gemini_web_canned_replies` setting); changes apply to the next request.
Account connection tests use the same built-in and custom phrases and report
a failed test instead of treating these replies as successful answers.

To find new phrases, every reply of at most 300 characters is logged in full as
`gemini_web.short_reply` with `canned`, `generation_failed` and `suspected`
flags. A reply that is
canned, or short and worded like a failure, also has its raw response body
saved under `$DATA_DIR/gemini-web-diagnostics/` (the newest 200 files are
kept; the prompt and credentials are never saved). Each request also logs
`gemini_web.completion_timing` with the time to headers, first frame and first
text.

Function calling is emulated, because the browser endpoint has no tool API.
Declared tools (`functionDeclarations`, Claude `tools`, OpenAI `tools`) and
`toolConfig` / `tool_choice` are described in the prompt, earlier calls and
results are replayed as a tagged transcript, and the model answers with
`<tool_call>` JSON blocks. The gateway converts blocks that name a declared
tool into native function calls and leaves anything else as text. While a call
is being generated its markup is held back; Claude streams receive `ping`
events and Chat Completions streams receive SSE comments in the meantime.
Plain conversations without tools keep the original prompt format.

Text prompts are supported. Image/file parts (including images inside tool
results) and OpenAI Responses `/v1/responses` still require a later protocol
extension.

The browser protocol details follow the public behavior documented by the
[geminiweb2api README](https://github.com/juangchuank-ops/geminiweb2api): the
provider discovers the `/app` session values, sends the form-encoded
`StreamGenerate` request, and parses Google's length-prefixed `wrb.fr` frames.
The Sub2API adapter is an independent implementation and does not import that
project.
