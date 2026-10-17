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

The current implementation buffers the browser response before emitting a
single provider response chunk. Text prompts are supported. Image/file parts,
browser tools, OpenAI Responses `/v1/responses`, and a full incremental
upstream stream still require a later protocol extension. The request path
persists cookies rotated by the upstream response; a periodic background
Cookie refresh probe is not included yet.

The browser protocol details follow the public behavior documented by the
[geminiweb2api README](https://github.com/juangchuank-ops/geminiweb2api): the
provider discovers the `/app` session values, sends the form-encoded
`StreamGenerate` request, and parses Google's length-prefixed `wrb.fr` frames.
The Sub2API adapter is an independent implementation and does not import that
project.
