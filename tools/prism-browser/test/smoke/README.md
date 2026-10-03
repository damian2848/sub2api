# Real Chromium offline smoke

```sh
npm ci --ignore-scripts
npx playwright install chromium
npm run test:smoke
```

A compatible existing local Chromium can be selected with
`PRISM_CHROMIUM_EXECUTABLE=/absolute/path/to/chromium npm run test:smoke`.
A missing or failed browser is a test failure, never a skipped test. No global
package installation is needed. The browser download is setup, not part of the
smoke run; the run itself uses only local fixtures.

The suite drives the production `BrowserSession` with real Chromium, its native
DOM selectors and its real request interception/response observer. A local HTTPS
server imitates the minimum Prism editor contracts: fake OAuth identity, Statsig
catalog readiness, project creation, sandbox sync and heartbeat, native Enter
submission, status/stop, file selection/upload and native attachment references.
It checks exact input/model/effort rewriting, request guards, typed failures,
no implicit native-start replay, cancellation, project separation and overlapping
requests. Experimental account-bound multiplexing is also exercised: two live
turns close their submission UI pages, continue on one JSON resident page, stop
only a cancelled turn and lazily create a new isolated-project UI page. Separate
account pollers retain distinct cookies and reject cross-account registration.
Resident idle-health checks perform a real authenticated request, refresh detached
workers and invalidate an unexpected account identity. Sandbox sync/heartbeat
fixtures deliberately leave their response bodies unread by the UI to cover
response observation independent of client consumption, including the backend
heartbeat path.

The suite also goes end-to-end through local management/user HTTP endpoints,
`createPrismServer`, `AccountPoolManager`, its durable project registry and real
Chromium with multiplexing both off and on. It checks one readiness probe per
worker, cross-worker reuse of the same gateway key/session, cross-key separation,
fresh anonymous projects, bootstrap-project exclusion, runtime metrics/resource
health wiring and resident reservations counting against worker capacity.
Cache is measured with an actual cacheable script loaded twice, server
request counts and Chromium's `Network.requestServedFromCache` event; no
`page.route` mock fulfillment is involved.

Chromium uses a deny-by-default CONNECT proxy that accepts **only**
`prism.openai.com:443` and tunnels it to the loopback fixture. All other proxy
destinations are blocked rather than forwarded. Keeping the production HTTPS
origin exercises its real cookie and same-origin behavior while fake credentials
never reach an external server. The self-signed certificate/key under `fixtures/`
are public test fixtures, not production credentials. TLS verification is relaxed
only for this launched test browser. No production account or OAuth token is read.

This is a reproducible contract/regression test, not evidence of current live
Prism markup, live service latency, billing correctness or production capacity.
