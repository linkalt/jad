# Phase p5 merged findings — the provider edge: `openaiapi`, `openaioauth`, `openaiauth`, `claudeauth`, `anthropicadapter`, `responses`, and the three `cmd/*` callers

- phase: `p5`, merged at HEAD `e3d6904` (go1.27.3 linux/amd64). Worktree before and after this merge: `M EXECUTOR.md`, `M MASTER.md`, `M audit_plan.md`, untracked `REVIEWER.md`, `REVIEWER4.md`, `audit_findings/`, `nodir/`; `git diff --check` exit 0. No repository file was changed — no source, config, `sample/`, or git state. The only repository file written is this one; probes lived in `/tmp/p5merge` and are removed at the end.
- inputs: `p5_r1.md` (192 lines, 10 findings, 6 hypotheses), `p5_r2.md` (195, 9, 4), `p5_r3.md` (167, 10, 7), `p5_r4.md` (130, 9, 1). Total **38 filings** and 18 raw hypotheses.
- **r4 left no scratch:** `/tmp/r4` does not exist and r4 has no memory record in this session. Its report therefore cannot be checked against its own probes, and its two "[reproduced]" items were re-reproduced here instead (§10). See C7 and §11.
- scope re-measured in this merge (`wc -l`, non-test): `internal/anthropicadapter` 19 files/3,233 lines, `internal/openaiauth` 9/1,226, `internal/claudeauth` 8/1,234, `internal/openaiapi` 3/538, `internal/openaioauth` 2/229, `internal/responses` 2/62 — 43 library files, 6,522 lines — plus `cmd/anthropic-adapter` 1 file, `cmd/claude-auth` 3 and `cmd/openai-auth` 3. r4 says 53; the four reviewers otherwise agree on the scope.
- **Verification outcome: 38 reviewer filings → 22 merged findings; 0 disproved as facts; 4 probes executed in this merge (one `openaioauth`, one `openaiapi`, two `anthropicadapter`), 18 findings confirmed by reading source at `e3d6904`.** Three filings are refuted as *defects* while their facts hold (P5M-06 `provider_network`, P5M-17 duplication, P5M-20 refresh-under-lock), one is a measurement note with no defect claim (P5M-22). Six claims are corrected (C1-C6): both "medium" `provider_network` filings describe behaviour that `DECISIONS.md` records as **not covered**; r2 s medium on the credential-path leak is lowered to the class rating this audit already uses; r4 s four mediums split 2 kept / 2 lowered; the A-17 record (ACCEPT-LOW, reasoned from "the default is loopback") no longer covers the default configuration after r1 s loopback probe; r1 s line citations are again unreliable.
- fixes assumed and not re-reported, each checked with `git log -1 --format=%s` in this merge: `5e5df51` (provider stream idle timeout), `0bf03a1` (`provider_network` forces the address family), `f0ecac6` (warn on a non-loopback adapter bind), `5a34549` (adapter `upstream.extra_body`), `bd26ba6` + `77b7213` (audit test and batch commits). r3 cites a `4f956b5` that does not exist in this history.

## 1. Counts

| measure | count |
|---|---|
| reviewer findings filed | 38 (r1 10, r2 9, r3 10, r4 9) |
| merged findings | 22 (every filing carried; no filing dropped as prior art; no filing split) |
| entries that merge several filings | 11, holding 27 references: P5M-04 (4 filings), P5M-02 (3), P5M-06 (3), P5M-09 (3), P5M-01 (2), P5M-03 (2), P5M-07 (2), P5M-08 (2), P5M-10 (2), P5M-12 (2), P5M-17 (2). The other 11 entries hold one filing each. |
| consensus (2+ reviewers) | 11: P5M-01 (r1, r4), P5M-02 (r1, r3, r4), P5M-03 (r2, r4), P5M-04 (r1, r2, r3, r4), P5M-06 (r2, r3, r4), P5M-07 (r1, r3), P5M-08 (r2, r3), P5M-09 (r1, r4, r4), P5M-10 (r1, r4), P5M-12 (r2, r3), P5M-17 (r1, r2) |
| unique (one reviewer) | 11: r1 3 (P5M-15, P5M-16, P5M-22), r2 3 (P5M-05, P5M-13, P5M-14), r3 4 (P5M-18, P5M-19, P5M-20, P5M-21), r4 1 (P5M-11) |
| severity as merged: critical / high / medium / low | 0 / 0 / **3** / 19 |
| severity as filed (38 filings) | critical 0 / high 0 / medium 10 (r1 2, r2 2, r3 2, r4 4) / low 28 |
| mediums: kept / lowered | 10 filed → 5 kept (r1-04, r3-02 and r4-09 in P5M-02; r4-08 in P5M-01; r2-02 in P5M-05) / 5 lowered (r2-01 → P5M-03, r4-02 → P5M-04, r3-01 + r4-04 → P5M-06, r1-07 → P5M-17) |
| r4 s own severity column | 9 filings: 4 medium, 5 low — **2 mediums kept, 2 lowered**; its 2 "[reproduced]" lows re-reproduced here |
| verification: executed in this merge / confirmed by reading / disproved as fact / unverified | 4 probes → 4 findings executed (P5M-01, P5M-05, P5M-10, P5M-11), 18 read-confirmed, 0 disproved as fact, 0 unverified |
| every unique (11) and every medium (3) finding verified — 13 distinct entries | 13 confirmed as facts; 2 of the uniques (P5M-20, and P5M-06 among the mediums) refuted as defects and kept as notes; 0 left unverified |
| claims corrected | 6 (C1-C6), plus r4 s severity column as a whole (C3) and r1 s line citations (C6) |
| contradictions recorded | 10 (C1-C10) |
| performance hypotheses | 18 raw (r1 6, r2 4, r3 7, r4 1) → 9 groups for measurement (§9); **none measured here** |
| findings added by this merge | 0 |
| repeats of prior art | 6: P5M-02 is A-17/D-7 (ACCEPT-LOW, open); P5M-06 is the `provider_network` "Not covered" list; P5M-08 is P1-R4-03 (low, unfixed); P5M-09 is X-1 (REJECT) with new specific evidence; P5M-17 is F-26(a) (not merged by user decision); P5M-18 is A-4 rest (documented "Not done here"). None is counted as a new class. |

`sev` below: **kept at medium** = P5M-01, P5M-02, P5M-05; everything else low. Per reviewer (entries, not filings): r1 10 entries from 10 filings — 7 consensus, 3 unique, 2 medium (1 kept); r2 8 entries from 9 filings, 5 consensus, 3 unique, 2 medium (1 kept); r3 9 entries from 10 filings — 5 consensus, 4 unique (1 refuted as a defect), 2 medium (both lowered); r4 6 entries from 9 filings — 6 consensus, **0 unique**, 4 medium (2 kept, 2 lowered).

## 2. Merged findings

`found by` lists the reviewer filings that map here. `sev` is the coordinator rating; `filed` is what the reviewer(s) said, one letter per filing in the order of `found by`. Status: **R** = this merge executed a probe that reproduced it, **S** = confirmed by reading source at `e3d6904`. Locations are the ones verified in this merge.

| id | title | sev | filed | lens | location | found by | status |
|---|---|---|---|---|---|---|---|
| P5M-01 | The OAuth transport re-attaches the access token and account id on every redirect hop, including a cross-origin one the Go client has just stripped | medium | l,m | B | `openaioauth/transport.go:23-49` (`RoundTrip` clones the request, deletes and re-sets `Authorization`, `chatgpt-account-id`, `X-Openai-Fedramp`, then delegates to `Base`); it sits *below* `http.Client`, so it runs once per hop; wiring `app/resolver.go:504-507` (no `CheckRedirect`) | r1-01, r4-08 | R |
| P5M-02 | The adapter authenticates no caller, so with `credential_file` set it spends the subscription token for anybody who reaches the port — non-loopback by misconfiguration, and loopback from a browser page (no `Host`, `Origin` or content-type check) | medium | m,m,m | B | `anthropicadapter/credential.go:112-126` (`upstreamToken` has no caller check at all when `credentialPath != ""`), `handler.go:35-78` (`create` calls `upstreamToken` before reading the body), `cmd/anthropic-adapter/main.go:37,70-77` (`net.Listen`, `warnIfExposed` warns and serves), `config.go:219-222` | r1-04, r3-02, r4-09 | S (r1 probe) |
| P5M-03 | A credential error puts the absolute credential-file path into the caller-visible message (adapter 401 `credential_unusable`, backend 422 at run start) | low | m,l | B | `anthropicadapter/credential.go:30-32` (`Error()` formats `Path`), `:112-126` (copied into `apiError.Message`), `openaioauth/credentials.go:37-39`, `app/oauth_client.go:29-38` (`StatusError{422, …Error()}`), `httpapi/handler.go:959-980` (`writeJSONError` writes `Message` verbatim); the contract it breaks is `DECISIONS.md:380` | r2-01, r4-03 | R (adapter half) / S (backend half) |
| P5M-04 | The adapter upstream SSE reader has no line or event size cap, while `openaiapi` inherits a 32 MiB one from the SDK | low | l,l,l,m | B/C | `anthropicadapter/stream_sse.go:19-61` (`bufio.Reader.ReadString` + `strings.Join`; the comment at `:19` says the absence is deliberate), `stream.go:87-137` (whole event unmarshalled); contrast the SDK at `openaiapi`: `ssestream.go:35` `scn.Buffer(nil, bufio.MaxScanTokenSize<<9)` | r1-06, r2-06, r3-08, r4-02 | S |
| P5M-05 | The adapter stops reading at `message_stop` and closes the body, so an HTTP/1.1 upstream pays a fresh TCP and TLS handshake every streaming round | medium | m | C | `anthropicadapter/stream.go:114-115` (`case "message_stop": t.complete()` ends `run`), `stream_handler.go:71` (`_ = stream.Close()`), `handler.go:26-29` (`&http.Client{}`, one pool per process) | r2-02 | R |
| P5M-06 | `provider_network` does not reach the adapter upstream dial or the token-refresh clients — **documented as not covered; a documentation gap, not a defect** | low | l,m,m | A/D | `anthropicadapter/handler.go:26-29` (nil client → `&http.Client{}`), `sdk.go:61-80` (timeout and headers only), `openaiauth/token.go:63-68` and `claudeauth/token.go:63-68` (`&http.Client{Timeout}` with nil `Transport`); the setting covers only `app/resolver.go:497-522`; `DECISIONS.md`, `provider_network` section: "Not covered: the anthropic adapter (a separate service with its own config), … and the OAuth token refresh clients" | r2-03, r3-01, r4-04 | S (refuted as a defect) |
| P5M-07 | `openaiauth` lets the lock helper create the credential directory 0755; `claudeauth` creates it 0700 first | low | l,l | B | `openaiauth/store.go:174-183` (`Update` goes straight to `sessionlock.PathFor`/`Acquire`), `sessionlock/lock.go:91` (`os.MkdirAll(dir, 0o755)`), contrast `claudeauth/store.go:160-163`; the files stay 0600 in both (`writeAtomic` temp file) | r1-02, r3-04 | R (r1 probe) / S |
| P5M-08 | The adapter prints the configured upstream URL verbatim on every start; `base_url` may carry userinfo and a query | low | l,l | B | `cmd/anthropic-adapter/main.go:53` (`, upstream %s`, `config.Upstream.BaseURL`), `anthropicadapter/config.go:268-274` (any parseable http(s) URL accepted) | r2-04, r3-03 | S (repeat of P1-R4-03) |
| P5M-09 | The sign-in callback treats `?error=` as final before checking `state`, so a stray request aborts the sign-in; `claudeauth` also arms the expected state only after opening the browser | low | l,l,l | A/B | `openaiauth/login.go:172-178` and `claudeauth/login.go:230-236` (the `error` branch sets `finished` and sends on `result` before the state compare), `claudeauth/login.go:79-95` (`callback.expectState` after `OpenBrowser`), contrast `openaiauth/login.go:63-67` | r1-05, r4-01, r4-07 | S |
| P5M-10 | Every `*url.Error` counts as a retryable transport error, including TLS verification and unsupported-scheme failures | low | l,l | A | `openaiapi/retry.go:55-92` (only `Op == "parse"` is excluded before the final `errors.As(err, &netError)`; `*url.Error` implements `net.Error`), consumer `client.go:124-160` | r1-03, r4-05 | R |
| P5M-11 | An oversized numeric `Retry-After` overflows `time.Duration` before the cap, giving a negative wait and an immediate retry | low | l | C | `openaiapi/retry.go:96-100` (`time.Duration(millis * float64(time.Millisecond))`; only NaN, Inf and negative are rejected), `:184-196` (`min(requested, MaxDelay)` keeps the negative), `sleepContext` → `time.NewTimer` fires at once | r4-06 | R |
| P5M-12 | `errors.Unwrap(err).Error()` in eleven credential-path sites assumes the inner error exists; `%w` with the unwrapped error also drops the operation and the path | low | l,l | D | `anthropicadapter/credential.go:46,53`, `openaioauth/credentials.go:76,83`, `openaiauth/store.go:161,206`, `claudeauth/store.go:147,163,196`, `openaiauth/status.go:68`, `claudeauth/status.go:60` — unreachable today (`os` errors are `*fs.PathError`) | r2-05, r3-05 | S |
| P5M-13 | `truncate` cuts an issuer description at a byte offset, so a multi-byte rune can be split and reach the terminal as invalid UTF-8 | low | l | D | `openaiauth/token.go:179-185` (used at `:166`), `claudeauth/token.go:177-183` (used at `:164`); the correct shape already exists at `anthropicadapter/log.go:117-119` (`clip`) | r2-09 | S |
| P5M-14 | `itoa` marshals an integer with `encoding/json` | low | l | C/D | `anthropicadapter/response.go:182-185`, called from `itemID` for every output item and every streamed id | r2-07 | S |
| P5M-15 | An event that arrives exactly at the idle deadline is reported as idle: `Stop` runs after `Next` returns and its result is ignored | low | l | A | `openaiapi/idle_guard.go:42-45` (`time.AfterFunc` sets `fired` then cancels), `client.go:92-103` (`guard.Stop()` after `stream.Next()`) | r1-09 | S (benign) |
| P5M-16 | A refresh stamps the new expiry with the clock read *after* the network call, so `expires_at` is late by up to one request | low | l | A | `claudeauth/refresh.go:74` (`current.Apply(tokens, now())` after `client.Refresh` returns), `claudeauth/store.go` (`ExpiresAt = now + expires_in`) | r1-10 | S |
| P5M-17 | The two auth packages are the same ~1.2k lines twice, and the copies already diverge — **open by user decision (F-26(a)); no fix proposed** | low | m,l | D | `openaiauth/{store,login,token,refresh,config,pkce,status,claims}.go` vs `claudeauth/…`; the divergence is P5M-07 and P5M-13; `audit_backend.md:149` records the decision | r1-07, r2-08 | S |
| P5M-18 | No `ResponseHeaderTimeout` on the provider transport, the adapter SDK client or the token clients (A-4 rest) | low | l | A | `app/provider_network.go:18-28` (the clone keeps `0`), `anthropicadapter/sdk.go:61-80` (whole-request timeout only), `openaiauth/token.go:63-68`, `claudeauth/token.go:63-68`; `DECISIONS.md:826-828` records it as not done | r3-09 | S (documented) |
| P5M-19 | `redirect_host` accepts any hostname, although the callback server only listens on loopback — a hostile value sends `code` elsewhere | low | l | B | `openaiauth/config.go:85,105-106` (`hostPattern` allows any bare host), `login.go:128-154` (`redirectURI` built from `config.RedirectHost`, listeners on `127.0.0.1` and `::1`); `claudeauth/config.go:91,124-128`, `login.go:186-212` | r3-06 | S |
| P5M-20 | `Refresh` holds the credential lock across the token exchange — **deliberate, documented in the code; not a defect** | low | l | A | `openaiauth/store.go:174-216` (`Update` holds `sessionlock` across `change`), `refresh.go:42-73` (the exchange is inside it); `TestConcurrentRefreshesAreSerialised` is the test that depends on it | r3-07 | S (refuted as a defect) |
| P5M-21 | `openai-auth` has no `--copy-code` while `claude-auth` does | low | l | D | `cmd/openai-auth/main.go:120-135` (`--force`, `--no-browser` only), `openaiauth/login.go:37-102` (no `CopyCode`); `cmd/claude-auth/main.go:126`, `claudeauth/login.go:24-27,88-95` | r3-10 | S |
| P5M-22 | Re-serialisation per retry attempt in `rawRequestOptions` — **measurement note, no defect claim** | low | l | C | `openaiapi/client.go:221-245`, applied at `:126` and `:139` | r1-08 | S (no defect) |

Severity rule applied: highest evidence, then the median severity of the filings, with departures recorded. **Kept at medium:** P5M-01 (r1 low, r4 medium — reproduced here and by both reviewers: the transport re-attaches a live bearer token to a host the Go client had just deliberately refused to send it to); P5M-02 (three mediums, and *raised* relative to prior art — see C5); P5M-05 (r2 medium, unique, reproduced here with a counterfactual). **Lowered to low:** P5M-03 (from r2 s medium: the identical class — absolute server paths in client-visible errors — was rated low in p3 as P3-09, and the reader here is the authenticated local user; the `DECISIONS.md:380` contradiction is recorded in C2 and raises its *priority*, not its severity); P5M-04 (from r4 s medium: three of the four filings say low, the upstream URL is operator configuration, the absence is stated in a code comment at `stream_sse.go:19`; it is the one item that becomes medium if the adapter is ever pointed at a shared gateway); P5M-06 (from r3 s and r4 s medium: `DECISIONS.md` lists exactly these two clients as not covered, so the code does what the record says — C1); P5M-17 (from r1 s medium: F-26(a) is open by user decision — C8).

## 3. Verification of the medium findings

### P5M-01 (medium, B) The OAuth transport re-attaches the credential on a cross-origin redirect — r1-01, r4-08 — **reproduced by execution here**

Probe `/tmp/p5merge/body1.go` (overlay onto `internal/openaioauth`), exit 0:

    first host=http://127.0.0.1:43261 second host=http://127.0.0.1:43473
    redirected request reached the second origin with Authorization="Bearer eyJhbGciOiJub25lIn0.…" Chatgpt-Account-Id="acct-1"

Two `httptest` servers, the first answering `307` to the second; the client is the production shape `&http.Client{Transport: &Transport{Base: http.DefaultTransport, Source: src}}`. Go strips `Authorization` when a redirect changes host, but `RoundTrip` runs again for the redirected request and re-sets every owned header (`transport.go:34-49`), because the transport sits below the client and cannot tell a redirect from an original request. The port counts as part of the host, so the probe needed no DNS.

Severity: **medium**. It needs a `base_url` (trusted configuration) that answers with a cross-host redirect, which is why r1 said low; but the failure mode is the exact one Go s defense exists to prevent — the feature that exists, showing the credential to a host the client refused — and the fix is small and local. r4 s framing (bind the credential to the configured origin) is the right one; r1 s `CheckRedirect` variant is a smaller equivalent. Both reviewers reproduced it independently, and so did this merge.

### P5M-02 (medium, B) The adapter has no caller authentication — r1-04, r3-02, r4-09 — **confirmed by reading, plus r1 s probe of the browser-shaped request**

Two exposures with one root cause.

1. **Non-loopback bind (A-17/D-7).** `run` does `net.Listen("tcp", config.Listen)` and serves; `warnIfExposed` only logs (`main.go:37,70-77`), `config.go:219-222` accepts any non-empty `host:port`, and `upstreamToken` with `credential_file` set never looks at the caller (`credential.go:112-126`). This is exactly the item `audit_audit.md:18` accepted as Low-Medium, and its fix (warning + opt-in caller token) is half done: the warning landed, the token did not. r3-02 and r4-09 re-file it; that half is a repeat, not new.
2. **Loopback is not a boundary (r1-04, new).** The handler checks neither `Host`, nor `Origin`, nor `Content-Type` (`handler.go:35-78`), and `create` spends the credential before it reads the body, so a request that a web page can send to `http://127.0.0.1:8788` gets the subscription token used with `200`. r1 probed exactly that request shape (`Content-Type: text/plain`, `Origin: https://evil.example`, `Host: rebind.evil.example:8788`) against a fake upstream. Caveats this merge adds: (a) current Chrome ships Private Network Access, which preflights a public-to-loopback request and would stop the simple-request variant; Firefox and Safari do not, and the DNS-rebinding variant (the page keeps its own origin after the name resolves to `127.0.0.1`) is only stopped by a `Host` check, which does not exist; (b) the caller cannot read the reply without CORS or rebinding, but the spend happens either way.

Severity: **medium, raised over the recorded ACCEPT-LOW** — the record justifies Low-Medium with "the default is loopback", and r1 s evidence removes that premise for the default configuration. See C5; the A-17 entry should be amended rather than left as it stands.

### P5M-05 (medium, C) The adapter abandons the upstream stream at `message_stop` — r2-02 — **reproduced by execution here, with the counterfactual**

Probe `/tmp/p5merge/body3.go` and `/tmp/p5merge/body3b.go` (overlay onto `internal/anthropicadapter`), both exit 0:

    3 streaming rounds with bytes after message_stop -> 3 new upstream TCP connections
    3 streaming rounds with no bytes after message_stop -> 1 new upstream TCP connections

Both probes run the real `Handler` (`adapterFor`) against an `httptest` upstream counting `http.StateNew`, with events flushed 2 ms apart and the first one leaving a comment/ping trailer plus 50 ms of life after `message_stop`. The only difference is the trailer, so the cause is isolated: `streamTranslator.run` returns at `message_stop` (`stream.go:114-115`), the rest of the body is never read, and `stream.Close()` (`stream_handler.go:71`) has to drop the connection instead of returning it to the pool. With nothing left in the body the same code reuses the connection, so the fix (a bounded `io.Copy(io.Discard, io.LimitReader(...))` before `Close`) is sufficient and needs no parser change.

Severity: **medium**. One extra handshake per provider round for every session that routes through the adapter at an HTTP/1.1 upstream (a local proxy or gateway — the shape the OAuth-to-adapter setup encourages). **Not measured:** whether the default upstream (`api.anthropic.com`, HTTP/2) is affected — an early body close resets the stream, not the connection — and the wall-clock cost of the handshake. Both belong to the measurement step (§9, PH1).

## 4. Verification of the unique findings and the other lows

### P5M-03 (low, B) The credential-file path reaches the caller — r2-01, r4-03 — **executed by r2 for the adapter, read-confirmed for the backend half**

The adapter half was reproduced by r2 s probe: a 401 whose message is `Claude OAuth credential /tmp/…/claude-oauth.json: access token expired at …`. The chain is `loadCredential` → `credentialError{Path, Reason}` → `Error()` (`credential.go:30-32`) → `apiError.Message` (`:124`) → `writeError`, and `create` calls `upstreamToken` before reading the body, so the path comes back even for an empty request. The backend half is read: `checkOAuthCredential` wraps `CredentialError.Error()` in a `StatusError{422}` (`app/oauth_client.go:29-38`, reached from `resolver.providerAPIKey`), and `writeJSONError` writes `Message` verbatim (`httpapi/handler.go:959-980`). **This contradicts `DECISIONS.md:380`** — "The client sees a generic failure … The file path and reason go to the server log" — so it is a defect, not a decision challenge (C2). Severity stays low: same class as P3-09, and the reader is the local user; it is the highest-priority low in §8 because it also breaks a written contract.

### P5M-04 (low, B/C) The upstream SSE reader has no size cap — r1-06, r2-06, r3-08, r4-02 — **confirmed by reading**

`readSSE` (`stream_sse.go:19-61`) reads with `bufio.Reader.ReadString('\n')` and joins every `data:` line of one event. The comment above it (`:19`) reads "Lines are read without a length limit: a tool input can be large" — deliberate, like P3-14, and unlike the OpenAI path the SDK bounds at `bufio.MaxScanTokenSize<<9` (32 MiB, read in the module cache of `openai-go/v3 v3.74.0`). The only other bound is `errorBodyLimit` (1 MiB) for a non-SSE upstream error body. A compromised or substituted `base_url` can therefore make the adapter buffer without limit, and `stream_retry` replays it. Not reachable by a caller of the adapter. Low.

### P5M-06 (low, A/D) `provider_network` does not cover the adapter dial or the token clients — r2-03, r3-01, r4-04 — **confirmed as a fact, refuted as a defect**

Confirmed by reading: `NewHandler(config, nil)` builds `&http.Client{}` (`handler.go:26-29`), `newAnthropicClient` adds only the base URL and `WithMaxRetries(0)` (`sdk.go:15-24`), the adapter schema has no network key, and both token clients build `&http.Client{Timeout: …}` with a nil `Transport` (`openaiauth/token.go:63-68`, `claudeauth/token.go:63-68`). `provider_network` is honoured only by `app.providerHTTPClient` (`resolver.go:497-522`), confirmed by r1 and r2 through the three `app` tests (exit 0). **But `DECISIONS.md`, `provider_network` section, ends with "Not covered: the anthropic adapter (a separate service with its own config), … and the OAuth token refresh clients."** The code matches the record; r2-03 reached the same conclusion and filed it as a documentation gap. Two reviewers rated it medium on the assumption that the setting promises more than it delivers — it does not promise this. Kept at **low** as a one-line documentation improvement in the adapter README and schema (C1).

### P5M-07 (low, B) `openaiauth` lets the credential directory be created 0755 — r1-02, r3-04 — **executed by r1, read-confirmed here**

`openaiauth.Store.Update` calls `sessionlock.PathFor`/`Acquire` without creating the directory first, and `openLockFile` creates it with `os.MkdirAll(dir, 0o755)` (`sessionlock/lock.go:91`). `claudeauth.Store.Update` creates it `0o700` first (`store.go:160-163`) and says why in a comment. r1 s probe (umask 022) saw `-rwxr-xr-x` for `openaiauth` and `-rwx------` for `claudeauth`; the file itself is 0600 in both, so the exposure is the listing (the credential file name, `.bak`, the lock file). `DECISIONS.md` claims directory 0700 for the openai side too, so this is a missed copy (P5M-17 s divergence). Low.

### P5M-08 (low, B) The adapter prints the upstream URL verbatim — r2-04, r3-03 — **confirmed by reading; a repeat of P1-R4-03**

`cmd/anthropic-adapter/main.go:53` prints `config.Upstream.BaseURL` on every start, and `config.go:268-274` accepts any parseable http(s) URL, including userinfo and a query. Severity low, and it is the same finding p1 filed as P1-R4-03 (low, unfixed); it is carried here so the p5 list is complete and so the fix lands once.

### P5M-09 (low, A/B) The `?error=` callback aborts the sign-in; claudeauth arms its state late — r1-05, r4-01, r4-07 — **confirmed by reading**

In both packages the `error` branch of `handle` sets `finished` and sends the result before the state comparison, so `GET /auth/callback?error=x` from any local process (or a page that can reach loopback) ends the flow with "the sign-in was refused by the issuer". No secret is exposed; the cost is a restarted sign-in. The claudeauth ordering (`expectState` at `login.go:92`, after `OpenBrowser` at `:80`) means a redirect that arrives inside that window meets `c.state == ""` and is answered `400`, which the browser does not retry — the window is one line wide, so practically unreachable, and openaiauth already does it in the right order (`login.go:63-67`). The general hardening class was **REJECTed** as X-1 (`audit_audit.md:41,178`); these two specific defects are narrower and are confirmed here, so they are kept as low with X-1 s reasoning attached (C4).

### P5M-10 (low, A) Every `*url.Error` counts as retryable — r1-03, r4-05 — **reproduced by execution here**

Probe `/tmp/p5merge/body2.go` (overlay onto `internal/openaiapi`), exit 0:

    isRetryableError(TLS unknown authority) = true
    isRetryableError(unsupported scheme) = true

The classifier excludes only `context.Canceled`, a `Permanent()` error and `*url.Error` with `Op == "parse"` before the final `errors.As(err, &netError)`; `*url.Error` implements `net.Error`, so a certificate or scheme failure waits through every backoff. Effect: retry notices for an unrecoverable error, and a slow failure. r1 and r4 both reported it; r4 reproduced it with its own overlay.

### P5M-11 (low, C) An oversized `Retry-After-Ms` becomes a negative wait — r4-06 — **reproduced by execution here, with a second value**

Same probe, exit 0:

    Retry-After-Ms=1000  -> retryAfter=1s ok=true; retryDelay=1s negative=false
    Retry-After-Ms=1e300 -> retryAfter=-2562047h47m16.854775808s ok=true; retryDelay=-2562047h47m16.854775808s negative=true
    Retry-After-Ms=1e18  -> retryAfter=-2562047h47m16.854775808s ok=true; retryDelay=-2562047h47m16.854775808s negative=true

`retryAfter` validates the float as finite and non-negative and then multiplies by `float64(time.Millisecond)` in `time.Duration`; an out-of-range float converts to the minimum `int64` on this platform, `min(negative, MaxDelay)` keeps it, and `time.NewTimer` fires immediately. **1e18 is a plausible value for a provider bug**, not only the absurd 1e300, so the retry storm r4 describes is more reachable than its example suggests. Low (`Retry-After` is provider-controlled and the wait only multiplies requests the client already intended to make), fixed by capping in the original unit before the conversion.

### P5M-12 (low, D) `errors.Unwrap` in the credential paths — r2-05, r3-05 — **confirmed by reading**

Eleven sites in six non-test files (`grep -n "errors.Unwrap" internal/`), all on `os` errors that are `*fs.PathError` today, so nothing panics; a future non-wrapping error would. `%w` with the unwrapped error also drops `*PathError s` operation and path from the message. Hardening only — the neighbouring `credentialError.Reason` already shows the safe shape.

### P5M-13 (low, D) `truncate` can split a rune — r2-09 — **confirmed by reading**

Both copies cut at `text[:limit] + "…"` (`openaiauth/token.go:182`, `claudeauth/token.go:180`) after collapsing whitespace, so a UTF-8 description from the issuer can end mid-rune. The adapter solved the same problem correctly in `log.go:117-119`. Cosmetic, terminal-only.

### P5M-14 (low, C/D) `itoa` uses `encoding/json` — r2-07 — **confirmed by reading**

`response.go:182-185` marshals an `int` and discards the (impossible) error, on every output item and every streamed item id. `strconv.Itoa` is the same result with no allocation. Trivial.

### P5M-15 (low, A) An event at the idle deadline is reported as idle — r1-09 — **confirmed by reading; benign**

`client.go:92-103` calls `guard.Stop()` after `stream.Next()` returns, and `Stop` ignores `timer.Stop s` result while `fired` is set by the `AfterFunc` callback. If the timer fired in that window the context is already cancelled, so the next `Next()` fails and `guard.Err(parent)` reports `StreamIdleError` for a stream that had just produced an event. The stream had been silent for the whole timeout anyway, so the visible effect is a boundary-case failure. No data race (`fired` is atomic). No action recommended; if one is wanted, check `fired` when `Stop` returns false.

### P5M-16 (low, A) The refresh stamps a late expiry — r1-10 — **confirmed by reading**

`claudeauth/refresh.go:74` passes `now()` to `Apply` after `client.Refresh` returns, so `expires_at` is late by the request latency (up to `http_timeout_ms`, default 30 s). The adapter default `expiry_margin_ms` of 300 s absorbs it; with a margin of 0 a token could be sent one latency after it died. Fix: read the clock before the call.

### P5M-17 (low, D) The auth packages are duplicated, and the copies diverge — r1-07, r2-08 — **confirmed by reading; open by decision**

`writeAtomic`, the callback server, PKCE, `truncate`, `firstNonEmpty`, `unwrapURLError`, `ExpandAuthPath`, the claim parsing and the token-error mapping exist two to four times. The divergence is not hypothetical: P5M-07 is a fix that landed in one copy, and P5M-13 is a helper that differs from the correct one in the adapter. But `audit_backend.md:149` records the user decision — F-26(a) is "NOT merged: the auth commands are unofficial and will be removed later" — so this is a re-confirmation of an open decision, not an actionable finding. r1 rated it medium; **low**, no fix until the packages are touched for another reason.

### P5M-18 (low, A) No `ResponseHeaderTimeout` anywhere in the phase — r3-09 — **confirmed by reading; documented**

The cloned provider transport keeps `ResponseHeaderTimeout: 0`, the adapter bounds a request only by a whole-request context timeout plus the SDK option, and the token clients use `http.Client{Timeout}`. The idle guard ends the silent-after-headers case; a trickling header is bounded only by the whole-request timeout. `DECISIONS.md:826-828` records this as deliberately not done ("The idle timeout already ends the silent-after-headers case; the header timeout needs a transport clone … that also changes the OAuth and `http_debug` wrappers"). Low, documented, no action unless the wrapper chain is touched.

### P5M-19 (low, B) `redirect_host` accepts any host — r3-06 — **confirmed by reading**

`hostPattern` (`openaiauth/config.go:85`) is a bare-host shape check; `finish()` accepts `example.com`; `listenForCallback` still binds `127.0.0.1` and `::1` but builds `redirect_uri` from `config.RedirectHost` (`login.go:148`). A config that names an attacker host sends the `code` there in the user s browser; PKCE keeps the exchange from completing, and the value comes from local config, which is why X-1 (`audit_design1.md:274`) called for a loopback validation rather than a rejection. Low; the check is three lines.

### P5M-20 (low, A) `Refresh` holds the file lock across the network call — r3-07 — **confirmed, refuted as a defect**

`store.Update` takes the `sessionlock` flock and runs `change` inside it, and `Refresh` performs the token exchange in `change` (`refresh.go:42-73`). That is required for a rotating refresh token (the code says so, and `TestConcurrentRefreshesAreSerialised` passes on it), and no deadlock exists because `Acquire` honours the context. r3 itself recommends keeping it. Carried as a note so the trade-off is not re-filed: a slow issuer blocks a second `login --force`/`refresh` for up to the HTTP timeout like B-C2 s 25 ms poll, which `audit_audit.md` already rejected.

### P5M-21 (low, D) `openai-auth` has no `--copy-code` — r3-10 — **confirmed by reading**

`runLogin` in `cmd/openai-auth/main.go:120-135` registers `--force` and `--no-browser` only, and `openaiauth.LoginOptions` has no `CopyCode`; the claude side has both (`cmd/claude-auth/main.go:126`, `claudeauth/login.go:24-27`). Whether the ChatGPT sign-in can work without a local loopback redirect is unknown (the Codex flow is loopback-shaped), so this is either a missing flag or a missing sentence. Low, documentation-first.

### P5M-22 (low, C) `rawRequestOptions` re-serialisation — r1-08 — **confirmed, no defect claim**

`client.go:221-245` builds one `WithJSONSet` per top-level key, and the option set is applied again for every retry attempt (`:126`, `:139`). Nothing is wrong; the cost is unmeasured and belongs in §9 (PH4). r1 filed it "only so it is not lost", and this merge keeps it in that role.

## 5. Contradictions, disagreements and refuted claims

### C1 (fact, two reviewers) r3-01 and r4-04 report `provider_network` not reaching the adapter as a medium defect — **the fact holds, the verdict does not; `DECISIONS.md` records it as not covered**
`DECISIONS.md`, `provider_network` section, final bullet: "**Not covered:** the anthropic adapter (a separate service with its own config), the `fetch` tool …, MCP, `decide`, and the OAuth token refresh clients." r2-03 found the same behaviour and filed it as a documentation gap; r3-01 quoted the same sentence and then rated it medium against it; r4-04 does not mention the sentence at all. A reviewer may disagree with a decision, but must then argue against it — neither does. Downgraded to low (P5M-06) and turned into a documentation item. The related half (`provider_network` *is* honoured by the backend provider client, including through the OAuth wrapper) was confirmed by r1 and r2 with the three `app` tests, exit 0.

### C2 (severity, and a broken contract) r2-01 rates the credential-path leak medium — **lowered to low, but it is the highest-priority low because it contradicts a written decision**
Same class as P3-09 (absolute server paths in client-visible error text), merged there at low. r2 s argument for medium is that the adapter authenticates no caller; that is P5M-02 s severity, not this one — the path is only useful after the caller has already been able to spend the credential. What does raise it is `DECISIONS.md:380` ("The client sees a generic failure … The file path and reason go to the server log"), which the code contradicts. Priority 4 in §8, severity low.

### C3 (severity column) r4 s four mediums — **two kept, two lowered; and its coverage is the thinnest of the four**
P5-R4-08 (redirect) and P5-R4-09 (A-17) are kept; P5-R4-02 (SSE cap) and P5-R4-04 (`provider_network`) are lowered for the reasons in §2 and C1. r4 announces "all 53 scoped non-test files" and produces **no unique finding at all** — every one of its nine filings is already in another report — while its two "[reproduced]" items (retry classification, `Retry-After` overflow) do hold and were re-reproduced here. So r4 s value is corroboration plus one genuinely new detail (the `1e18`-scale overflow consequence and its request-storm framing, which this merge strengthened).

### C4 (prior art) r4-01 and r4-07 re-open the callback hardening that X-1 REJECTed — **the class stays rejected; two narrower defects are confirmed**
`audit_audit.md:41,178` rejected "OAuth callback hardening" (loopback bind, 10 s header timeout, constant-time state, tiny pages). Neither r4 filing re-argues that. What they add is specific and confirmed by reading: the `?error=` branch ends the sign-in before the state check (r1-05 found it independently), and `claudeauth` arms its expected state one line after opening the browser. Both are kept as P5M-09 at low; nothing else in X-1 is reopened.

### C5 (prior art) r1-04 s loopback probe against the recorded ACCEPT-LOW of A-17 — **the new evidence is real and the record needs amending; severity raised to medium**
`audit_audit.md:18,103-107` accepted A-17 at Low-Medium with the reasoning "the default is loopback; the proposed refusal would break a legitimate container bind". r1 s evidence is that loopback is not a boundary against a browser page or a DNS-rebinding origin, so the accepted reasoning covers a case that does not exist. The recommendation (warning now, opt-in caller token later) also remains half-implemented — the warning exists, the token does not. **Recommendation: amend the A-17 entry rather than re-rate it in place** (decision 2 in §12), and treat the caller-token half as the fix.

### C6 (citations) r1 s line numbers are again unreliable — **its functions, quoted strings and probe results are correct**
Examples: `readSSE` is cited as `stream_sse.go:111-155`, it is `20-61`; `Transport.RoundTrip` as `transport.go:197-222`, it is `23-49`; `upstreamToken` as `credential.go:278-294`, it is `112-126`. The same problem was recorded as C8 in p3. Use r1 for facts and this report for locations.

### C7 (data) r4 s scratch and memory record are missing — **its two probes were re-run here, nothing else could be checked**
`/tmp/r4` does not exist (the directory listed in its "Commands run" section), and there is no memory record for r4. Its two overlay commands cannot be inspected, only repeated: this merge reproduced both results with its own probes (§10). No r4 claim is left unverified, but no r4 claim is corroborated by anything except the other three reviews.

### C8 (prior art) r1-07 rates the auth-package duplication medium — **lowered to low; the user has already decided F-26(a) stays open**
`audit_backend.md:149`: "F-26(a), the duplicated `openaiauth` and `claudeauth` code, is NOT merged: the auth commands are unofficial and will be removed later." r2-08 agrees and proposes no fix now. r1 calls 151 differing lines "close but already diverging" without connecting that to the decision. Kept as P5M-17 at low, with the divergence evidence (P5M-07, P5M-13) attached, because that is the argument that would reopen the decision — not the line count.

### C9 (framing) r3-07 files the refresh-under-lock behaviour as a finding — **it is deliberate and documented in the code it sits on**
`refresh.go:36-37`: "It holds the file lock for the whole exchange, so a second run waits and then sees the refresh token the first run stored." r3 s own suggested fix is "keep (documented trade-off)". Kept as P5M-20 so it does not return a third time.

### C10 (framing) r1-08 files a measurement as a finding with "no number" — **kept as a measurement note**
Nothing is wrong with `rawRequestOptions`; the concern is O(keys x body) per attempt (§9 PH4). Carried as P5M-22 with no severity claim beyond the label, so the list and §9 do not disagree.

**No reviewer s facts conflict with another s.** The four disagreements are severity or verdict of the same facts (C1, C2, C3, C5), and every one of them is resolved against the repository record rather than against a reviewer.

## 6. Reviewer-pair agreement

| pair | shared entries | notes |
|---|---|---|
| r1 + r2 | P5M-03 (different halves), P5M-04, P5M-12, P5M-17 | r2 corroborates r1 on the SSE cap and the `errors.Unwrap` sites; they differ on the credential path (r2 medium, r1 silent) and on duplication (r1 medium, r2 low) |
| r1 + r3 | P5M-02, P5M-04, P5M-07, P5M-08 (r1 by omission: it lists the URL leak nowhere), P5M-09 | strongest overlap on the auth-store discipline; r3 adds the startup URL that r2 also found |
| r1 + r4 | P5M-01, P5M-02, P5M-03, P5M-09, P5M-10 | the two executed probes for the redirect match exactly |
| r2 + r3 | P5M-04, P5M-06, P5M-12 | both reached "documented, not a defect" on `provider_network`; r3 did not act on it |
| r2 + r4 | P5M-03 (both halves), P5M-06 | r2 s probe is the only executed evidence for P5M-03 |
| r3 + r4 | P5M-02, P5M-06, P5M-04 | |

Three independent statements exist for P5M-02, P5M-04 and P5M-06; two for P5M-01, P5M-03, P5M-07, P5M-08, P5M-09, P5M-10, P5M-12, P5M-17. r4 contributes no entry that another reviewer did not also reach, so cross-reviewer overlap here is **weak corroboration**: as recorded in p3 C6, the retrieval index serves `audit_findings/`, so a later reviewer can reach an earlier report s wording without ever reading the code path.

## 7. Prior art handling

| prior item | status there | p5 filings | handling here |
|---|---|---|---|
| A-17 / D-7 adapter is an unauthenticated relay on a non-loopback bind | ACCEPT-LOW (Medium → Low-Med), fix = warning (done) + opt-in caller token (not built) | r1-04, r3-02, r4-09 | P5M-02, **raised to medium** because r1 s loopback evidence removes the reasoning the accept rested on; record to be amended (C5) |
| X-1 OAuth callback hardening | REJECT | r1-05, r4-01, r4-07 | P5M-09 kept at low for the two narrower, confirmed defects only (C4) |
| F-26(a) duplicated auth packages | open by user decision, not merged | r1-07, r2-08 | P5M-17 at low, no fix (C8) |
| A-4 rest: no `ResponseHeaderTimeout` on provider/token clients | "Not done here" in `DECISIONS.md:826-828` | r3-09 | P5M-18 at low, documented, no action |
| `provider_network` "Not covered" list | recorded in `DECISIONS.md` | r2-03, r3-01, r4-04 | P5M-06 at low, documentation only (C1) |
| P1-R4-03 adapter startup prints the upstream URL | low, unfixed | r2-04, r3-03 | P5M-08 at low, carried so the fix lands once |
| B-C2 25 ms lock poll rejected | rejected | r3-07 (adjacent) | P5M-20, refuted as a defect (C9) |
| P-018 / A-4 idle guard; P-011 stream event cost | measured, no action | r1 H2, r2 H03, r3 H03 | stay as hypotheses PH5 with the prior numbers attached |
| `openaioauth.Source.Load` 10.9 µs per request | measured in `audit_performance_plan.md` Phase 0 | r1 H3, r2 H04, r3 H-01, r4 s single hypothesis | PH6: closed by the existing measurement, no bench needed |

Nothing in p5 contradicts an earlier audit record except the code/`DECISIONS.md:380` mismatch in C2, which is a defect *against* the record rather than a disagreement with it.

## 8. Ranked list (severity, then evidence, then finders, then effort)

| rank | id | sev | evidence | finders | effort | why here |
|---|---|---|---|---|---|---|
| 1 | P5M-02 | medium | read + r1 probe | 3 | S/M | spends a real subscription credential; the default configuration is not safe against a browser; A-17 s accept reasoning is void |
| 2 | P5M-01 | medium | reproduced 3x (r1, r4, here) | 2 | S | a live bearer token reaches a host the client refused to send it to |
| 3 | P5M-05 | medium | reproduced here with counterfactual | 1 | S | one handshake per provider round through the adapter; fix is four lines and is already understood |
| 4 | P5M-03 | low | r2 probe + read | 2 | S | breaks a written privacy contract (`DECISIONS.md:380`); same class as P3-09 |
| 5 | P5M-11 | low | reproduced here | 1 | S | turns a provider bug into a retry storm; `1e18` makes it plausible |
| 6 | P5M-07 | low | r1 probe + read | 2 | S | a private directory made public by a missed copy; the sibling package shows the fix |
| 7 | P5M-10 | low | reproduced here | 2 | S | retries what cannot recover, with user-visible retry notices |
| 8 | P5M-09 | low | read | 3 | S | a stray request ends a sign-in; one ordering difference between the two packages |
| 9 | P5M-04 | low | read | 4 | S/M | unbounded upstream buffering; matters only if the adapter is pointed at a shared gateway |
| 10 | P5M-08 | low | read | 2 | S | a credential can sit in a startup log line |
| 11 | P5M-19 | low | read | 1 | S | a config value that can send the code elsewhere; PKCE limits the damage |
| 12 | P5M-06 | low | read + `DECISIONS` | 3 | S | a setting that reads wider than it is; one documentation line |
| 13 | P5M-16 | low | read | 1 | S | a late `expires_at`; absorbed by the 300 s margin |
| 14 | P5M-13 | low | read | 1 | S | cosmetic, terminal only |
| 15 | P5M-14 | low | read | 1 | S | trivial |
| 16 | P5M-18 | low | read + `DECISIONS` | 1 | S | a stall window already bounded by two other timeouts |
| 17 | P5M-12 | low | read | 2 | S | unreachable today, mechanical |
| 18 | P5M-21 | low | read | 1 | S/M | a missing flag or a missing sentence |
| 19 | P5M-15 | low | read | 1 | — | deliberate non-action; recorded so it is not re-filed |
| 20 | P5M-17 | low | read + decision | 2 | L | open by user decision |
| 21 | P5M-20 | low | read | 1 | — | refuted as a defect; recorded so it is not re-filed |
| 22 | P5M-22 | low | read | 1 | — | measurement note, no defect |

## 9. Performance hypotheses for the measurement step (grouped; **nothing measured here** — every group is measured in §12-bis)

18 raw hypotheses (r1 H1-H6, r2 H01-H04, r3 H-01…H-07, r4 s one) → 9 groups. "existing" means the benchmark is in the tree at `e3d6904` and was checked by name in this merge; "new" means nothing covers it. Every row is `[unmeasured]`.

| id | hypothesis | file:line | workload | benchmark | sources |
|---|---|---|---|---|---|
| PH1 | every streaming round through the adapter opens a new upstream connection when the upstream is HTTP/1.1 (P5M-05) | `stream.go:114-115`, `stream_handler.go:71` | N = 2 and 20 streaming rounds against one HTTP/1.1 fixture; the same against `httptest.NewTLSServer` for the h2 contrast | none for connection counts (use the probe shape of §10: an `httptest` server counting `http.StateNew`, events flushed 2 ms apart, with and without a trailer); timing counterpart `BenchmarkStreamThroughAdapter` (`anthropicadapter/adapter_benchmark_test.go:45`) | r2 H01 |
| PH2 | the adapter parses and re-encodes a request more than once per call | `request.go:26-35` (map + struct), `sdk.go:45,51-58` (`extra_body` unmarshal and re-marshal), `input.go:233-241`, `parts.go:72` | 1, 10 and 50 turns with 20 tools; a 20 MB image-bearing body for peak heap | existing `BenchmarkTranslateRequest` (`adapter_benchmark_test.go:27`), extended with the larger fixtures | r1 H5, r2 H02 |
| PH3 | the adapter allocates 8-12 objects per upstream delta in translation and in the sink | `stream_sse.go:19-61,100-120` (`event()` copies the map, marshals, concatenates), `stream.go:87-137` | 10 000 `content_block_delta` events of 5 tokens; one 1 MB tool-argument stream | new `BenchmarkTranslateStream` (canned SSE into `streamTranslator.run` with a recorder sink — listed as a gap in `audit_plan.md` §2) | r1 H4, r3 H-04 |
| PH4 | `rawRequestOptions` re-serialises the whole body once per top-level key, and again per retry attempt (P5M-22) | `client.go:221-245`, applied at `:126`, `:139` | a 5-50 MB `input`, 10-100 top-level keys, 0 and 3 retries | existing `BenchmarkStream` with a sized request, compared against `session`'s `BenchmarkBuildRequestWithLargeHistory` to see whether the provider layer matters at all | r1 H1, r3 H-06 |
| PH5 | per-event work in `Stream` (`json.Valid` on every already-parsed event, `guard.Reset`/`Stop` timer churn) | `client.go:92-110`, `idle_guard.go:52-62` | 1 000 and 10 000 SSE events | existing `BenchmarkStream` (`client_benchmark_test.go:14-42`); prior numbers P-011/P-026 already show 85 % of the per-event cost inside the SDK, so this is a closure unless a fix is proposed | r1 H2, r2 H03, r3 H-03 |
| PH6 | `openaioauth.Source.Load` stats, reads, decodes and parses two JWTs on every provider request | `credentials.go:67-119`, `transport.go:23-33` | 1 vs 100 sequential `Load` calls; one `RoundTrip` against an `httptest` server | **closed by an existing measurement**: 10.9 µs and 31 allocs per request (`audit_performance_plan.md`, Phase 0). No new bench; the freshness behaviour is the reason it exists | r1 H3, r2 H04, r3 H-01, r4 |
| PH7 | `anthropicadapter.loadCredential` re-reads and re-parses the credential per request | `credential.go:37-73` | a handler with and without `credential_file`, 1 and 100 requests | new (`BenchmarkCredentialLoad`), or extend the adapter benchmark harness | r3 H-02 |
| PH8 | `providerHTTPClient` builds a transport (and the runner cache never shares it across runner keys) whenever `provider_network`, OAuth or `http_debug` is on, so each such session pays its own handshake and keeps idle connections | `app/resolver.go:497-522`, `app/provider_network.go:18-28` | 20 sessions on `tcp4`, one round each; 1 vs 64 sub-agent runs | existing `BenchmarkStreamConnectionReuse` (`openaiapi/conn_reuse_benchmark_test.go`), plus a count of `newNetworkTransport` calls | r1 H6, r3 H-05 |
| PH9 | a refresh under the file lock holds a second `refresh`/`login --force` for up to the issuer timeout | `openaiauth/store.go:174-216`, `refresh.go:42-73` | cold path; two concurrent refreshes against a slow fake issuer | no benchmark needed unless A-4 s header timeout changes the bound | r3 H-07 |

Order for the measurement step (relative): **PH1 first** (it is the only hypothesis with a reproduced mechanism and a one-line fix; it decides decision 3), then PH2 and PH3 together (one harness, the adapter hot path), then PH4 and PH8 (the two "one extra copy or one extra transport" claims that a benchmark can close or drop), then PH7, then PH5 last (already bounded by P-011 and P-026). PH6 and PH9 need nothing.

## 10. Verification log (what I ran) and repository state

Repo state: `git rev-parse --short HEAD` → `e3d6904`; `git status --short` before and after: `M EXECUTOR.md`, `M MASTER.md`, `M audit_plan.md`, untracked `REVIEWER.md`, `REVIEWER4.md`, `audit_findings/`, `nodir/`; `git diff --check` → exit 0. No code, config, `sample/` or git state changed; the only repository file written is this report. Scratch: `/tmp/p5merge` (four probe test files, one overlay file, `notes.md`) — removed at the end.

Probes: one `go test -overlay` per package, mapped onto paths inside `internal/openaioauth`, `internal/openaiapi` and `internal/anthropicadapter`, so nothing was written in the tree. Every probe calls product code (`Transport.RoundTrip`, `isRetryableError`, `retryAfter`, `retryDelay`, `Handler` through `httptest`), never a copy.

| command | exit | observed |
|---|---|---|
| `go test -overlay /tmp/p5merge/overlay.json ./internal/openaioauth/ -run TestP5MergeRedirectReAddsToken -count=1 -v` | 0 | `first host=http://127.0.0.1:43261 second host=http://127.0.0.1:43473`; `redirected request reached the second origin with Authorization="Bearer eyJ…" Chatgpt-Account-Id="acct-1"` |
| `go test -overlay /tmp/p5merge/overlay.json ./internal/openaiapi/ -run TestP5MergeRetryClassificationAndOverflow -count=1 -v` | 0 | `isRetryableError(TLS unknown authority) = true`; `isRetryableError(unsupported scheme) = true`; `Retry-After-Ms=1000 -> 1s`; `1e300 -> -2562047h47m16.854775808s negative=true`; `1e18 -> -2562047h47m16.854775808s negative=true` |
| `go test -overlay /tmp/p5merge/overlay.json ./internal/anthropicadapter/ -run TestP5MergeUpstreamConnectionReusedAfterMessageStop -count=1 -v` | 0 | `3 streaming rounds -> 3 new upstream TCP connections` (trailer after `message_stop`) |
| `go test -overlay /tmp/p5merge/overlay.json ./internal/anthropicadapter/ -run TestP5MergeCounterfactual -count=1 -v` | 0 | `3 streaming rounds with no bytes after message_stop -> 1 new upstream TCP connections` |
| `git rev-parse --short HEAD`, `git status --short`, `git diff --check`, `wc -l` on the scope files and the four inputs, `grep -n "errors.Unwrap"` (11 sites/6 files), `grep -n` for `MaxScanTokenSize` in the module cache | 0 | counts and locations quoted in §1-§4 |

Read at `e3d6904` (targeted ranges, never a whole Go file): `openaioauth/transport.go:20-60`, `openaioauth/credentials.go:24-45`; `openaiapi/retry.go:50-225`, `client.go:85-112`, `idle_guard.go:30-60`; `anthropicadapter/handler.go:20-145`, `stream.go:100-160`, `stream_handler.go:16-85`, `stream_sse.go:19-120`, `credential.go:14-126`, `response.go:175-190`; `cmd/anthropic-adapter/main.go:30-80`; `openaiauth/store.go:150-180`, `login.go:128-200`, `token.go:165-190`, `config.go:84-112`; `claudeauth/store.go:145-170`, `login.go:70-100,220-250`, `refresh.go:30-90`, `token.go:55-75,175-185`, `config.go:88-130`; `cmd/openai-auth/main.go:115-150`; `app/oauth_client.go:25-45`, `app/resolver.go:455-480,490-525`; `sessionlock/lock.go:70-100`; `httpapi/handler.go:955-985`; `DECISIONS.md:376-382,398-410,820-835,990-1012`; `audit_audit.md:18,41,100-112,178`; `audit_backend.md:136-152`; `audit_findings/p1_r4.md:20,58,136`. **Not run, by design:** no `-race` sweep, no benchmark, no profile, no full suite, no real provider or issuer, no browser, no `npm`/frontend command, no CLI execution (fixed ports and real credentials are forbidden).

## 11. Coverage gaps and unverified items

### Coverage gaps
- **No `-race` run** (forbidden in this step and in the review step). The phase is mostly synchronous per request; the concurrency claims that remain are P5M-15 (`atomic.Bool` on the guard) and P5M-20 (flock across a network call), both read from code plus an existing passing test.
- **No real provider, issuer or browser.** Anthropic, `platform.claude.com`, `claude.ai`, `auth.openai.com` and `chatgpt.com` were never contacted (forbidden, and `DECISIONS.md:369,381,409` already says their behaviour is unconfirmed). P5M-02 s browser reachability rests on r1 s hand-built request plus the PNA caveat in §3; no browser was driven.
- **HTTP/2 not exercised.** P5M-05 and PH1 are measured on plain-text HTTP/1.1 `httptest` servers only. Whether an early body close costs anything against the default HTTP/2 upstream is unmeasured and is the first thing PH1 should settle.
- **No live TLS.** P5M-10 uses synthetic `*url.Error` values standing in for what `http.Client.Do` returns; no certificate was presented.
- **Frontend untouched.** Nothing in p5 has a `frontend_new` consequence; no `npm test`/`typecheck` was run.
- **`messages.jsonl` untouched.** The adapter translates request-scoped payloads and the auth tools never write transcripts, so no golden diff was run and none is needed for the merged findings. A fix to P5M-05 (drain before close) does not change any emitted event.
- **r4 s missing scratch** (C7) means the "fourth opinion" is corroboration only; if a genuinely independent fourth read is wanted, that scope needs a re-run with its notes kept.
- **`internal/responses`** is 62 lines of contract; all four reviewers called it sound and this merge read none of it beyond the retry-notifier shape quoted in a finding. **`anthropicadapter` translation details** (`input.go`, `tools.go`, `thinking.go`, `reasoning.go`, `cache.go`, `parts.go`, `anthropic_types.go`) were sampled by r1, r2 and r3 but not exhaustively traced by any of them, and not re-read here; the model tables and tool-name questions stay open by decision.
- **`internal/*/status.go` and `cmd/*` bodies** were read for the token-printing, exit-code and flag rules only (P5M-21), not line by line.

### Unverified (stays in the list, flagged)
- **P5M-02**: the non-loopback half is read; the loopback/browser half is r1 s probe of the request shape, not a browser, and the current-Chrome PNA behaviour is stated from general knowledge, not measured here.
- **P5M-05**: the connection count is reproduced; the **time** cost is not (PH1).
- **P5M-03**: the adapter 401 is executed by r2; the backend 422 is read end to end but no `app` fixture was driven, and no run was started.
- **P5M-04**: no oversized frame was actually fed to the reader; the absence of a cap is read from source and its comment.
- **P5M-07**: r1 measured the mode; this merge did not re-run the umask probe.
- **P5M-09, P5M-19**: no callback server was started (fixed ports are forbidden); both are read from the handler and the config validation.
- **P5M-15, P5M-16**: read only; neither window was forced.
- **P5M-11**: reproduced at the `retryDelay` boundary; the resulting request storm was not driven through a `Stream` loop.

## 12. Needs the user

### Decisions

1. **P5M-01 — how to stop the transport re-attaching the credential.** Two shapes: (a) bind the credential to the configured origin inside `RoundTrip` (compare `request.URL.Host` with the base URL host, return an error otherwise — r4); (b) set `CheckRedirect` to `http.ErrUseLastResponse` (or refuse a cross-host redirect) on the provider `http.Client` built at `app/resolver.go:504-507` (r1). **Recommendation: both, in that order, because they answer different questions** — (a) makes the transport safe for any caller that wraps it (the adapter path, a future sub-agent client), (b) makes the *client* refuse a redirect that the SDK probably should not follow at all. If only one, take (a): it is the layer that owns the credential. Test: the cross-origin redirect fixture in §10 becomes a `_test.go` in `openaioauth`. Effort S.

2. **P5M-02 + the A-17 record — the adapter needs a caller token, not just a warning.** `audit_audit.md:103-107` accepted A-17 with "the default is loopback" as the reason; r1 s probe shows a browser page or a rebinding origin reaches loopback too, and the recommended caller token was never built. **Recommendation: build the opt-in caller token that audit_audit already approved** — a config key, a constant-time compare as in `httpapi.(*Handler).authorized` (`httpapi/handler.go:433-441`, a hashed bearer token), and the rule that it becomes mandatory when `listen` is not loopback; keep the existing warning; on top of it add the two cheap checks the probe needs (`Content-Type: application/json` required, and reject a request whose `Host` is neither loopback nor the configured listen host). Then **amend the A-17 entry** in `audit_audit.md` (and its `DECISIONS.md` note) to say the loopback case is covered by the probe, so a fifth reviewer does not re-file it. Refusing a non-loopback bind outright (r3 s variant) stays rejected for the container reason already recorded. Effort M.

3. **P5M-05 — drain before close.** **Recommendation: yes, take the four-line fix** — `_, _ = io.Copy(io.Discard, io.LimitReader(upstream.Body, 64<<10))` immediately before `stream.Close()` on both the success and the failure return in `stream_handler.go`, plus the counterfactual test from §10 as the regression. It changes no emitted event, no parser behaviour and no transcript shape. Run PH1 first if you want the number in the commit message; the fix does not depend on it. Effort S.

4. **P5M-03 — the credential path in a 401/422.** **Recommendation: split the message, keep the path server-side** — `credentialError`/`CredentialError` get a client-facing `Message()` without `Path`, the existing `Error()` (with the path) goes to the logger, and one test per reader asserts the response body contains no `/`. This also restores `DECISIONS.md:380`, which currently describes behaviour the code does not have — so either fix the code or amend the decision; fixing is cheaper and matches the intent. Effort S.

5. **P5M-07 — the 0755 directory.** **Recommendation: copy the two-line `MkdirAll(dir, 0o700)` guard into `openaiauth.Store.Update`**, mirroring `claudeauth/store.go:160-163`, with a test beside the claudeauth one. Do **not** change `sessionlock.openLockFile` to 0700 in the same change: the lock directory is shared with session locks elsewhere, and a mode change there is a separate decision. Effort S.

6. **P5M-04 — cap the upstream SSE frame?** **Recommendation: yes, but as a bound, not a policy**: an `upstream.max_event_bytes` key (default 16 MiB, below the 32 MiB the SDK gives the OpenAI path) enforced in `readSSE`, failing the stream with a named code, schema and config test included. If you prefer no new key, at least cap at `max_request_bytes` (64 MiB) so the "deliberate, because a tool input can be large" comment has a number. It is the one low that becomes medium the day the adapter is pointed at a shared gateway. Effort S/M.

7. **P5M-10 + P5M-11 — the retry classifier and the overflow.** **Recommendation: one small change to `retry.go`**, with three regression cases in `retry_test.go`: classify `x509.UnknownAuthorityError`, `x509.HostnameError`, `tls.CertificateVerificationError` and an unsupported-scheme `*url.Error` as permanent **before** the `net.Error` test, and clamp the parsed wait to `MaxDelay` **in the original unit** before the `time.Duration` conversion (the same for numeric `Retry-After` and the rate-limit reset). Both are executed facts, both are cheap, and they belong together because they are the same function pair. Effort S.

8. **P5M-09 — the callback branch and the claudeauth ordering.** **Recommendation: do both, three lines each**, and keep X-1 rejected otherwise: compare `state` before acting on `error=` (RFC 6749 does send `state` with an error response; on a mismatch, answer 400 and leave the flow running), and move `callback.expectState(request.State)` in `claudeauth/login.go` to right after `NewAuthRequest`, as `openaiauth` already does. Add the four tests r4 names (missing state with `error`, wrong state with `error`, valid state with `error`, and a callback delivered before `OpenBrowser` returns). Effort S.

9. **P5M-06 + P5M-08 + P5M-18 — the three documentation items, in one pass.** **Recommendation: accept all three as documentation**, not code: state in `cmd/anthropic-adapter/README.md` and `sample/schemes/anthropic-adapter.schema.json` that `provider_network` covers only the backend-to-`base_url` hop and cannot constrain the adapter s second dial or the token refreshes (P5M-06); print `scheme://host` instead of the full `base_url` at startup (P5M-08 — that one is a one-line code change, and it is the repeat of P1-R4-03 so it should be closed); leave `ResponseHeaderTimeout` as `DECISIONS.md:826-828` has it until the wrapper chain is next touched (P5M-18). Effort S.

10. **P5M-17 — the duplication.** **Recommendation: keep it open.** F-26(a) is a user decision, the packages are scheduled for removal, and the two divergences that matter are already separate items (P5M-07, P5M-13). If one of the auth packages is touched for another reason, fold `writeAtomic` + the callback server + `truncate` into one small package then, not now. Effort L.

11. **P5M-12 + P5M-13 + P5M-14 + P5M-16 + P5M-19 + P5M-21 — the small batch.** **Recommendation: take the mechanical five now** (`errors.Unwrap` → `err` in the eleven sites, the rune-safe `truncate`, `strconv.Itoa`, the pre-call clock in `claudeauth/refresh.go`, the loopback check on `redirect_host`) **and decide P5M-21 separately**: whether the ChatGPT sign-in can work without a local redirect is a wire fact nobody in this project knows, so either add `--copy-code` or write the sentence that says why loopback is required. Do not bundle P5M-21 with the mechanical five.

12. **P5M-15 + P5M-20 + P5M-22 — the three "no change" items.** **Recommendation: close them as recorded, with one sentence each in the code comment that already explains the behaviour** (the guard s boundary, the lock-across-exchange trade-off, and the retry option cost). Nothing else.

13. **Severity confirmations.** Medium: P5M-01, P5M-02, P5M-05. Everything else low, with five mediums lowered (r2-01 → P5M-03, r4-02 → P5M-04, r3-01 + r4-04 → P5M-06, r1-07 → P5M-17) and two uniques refuted as defects (P5M-06, P5M-20). **Recommendation: confirm as merged**, with the one item that could reasonably go the other way being P5M-03 if you consider a documented privacy contract at the same level as a privilege boundary (in which case it is medium and rank 4 stays).

14. **Measurement go-ahead for §9 s 9 groups**, in the order given, plus **one `-race` pass on `internal/anthropicadapter`, `internal/openaiauth` and `internal/openaioauth`** after the P5M-01/P5M-05 batch — the phase has two reproduced credential/connection defects and one lock-across-network path, and the fix step should be able to say those packages are race clean rather than assume it.

### Coverage gaps that need your decision, not more review
- **r4 s scratch is gone and it filed nothing unique** (C7, C3). If the fourth opinion must be independent, that scope needs a re-run with notes kept; otherwise p5 s effective evidence base is three reviewers plus this merge s four probes.
- **P5M-02 s browser reachability** depends on Private Network Access rules that differ per browser and version; if you want this settled rather than reasoned, it needs a real browser, which no audit step is allowed to drive.
- **P5M-05 s cost against the default HTTP/2 upstream** is the one open question that changes whether the finding is worth a commit; PH1 answers it in minutes.

Scratch: `/tmp/p5merge` (four probe files, the overlay JSON, `notes.md`) — removed at the end of this step; nothing to remove from the tree.

## 12-bis Measured performance (the §9 groups, one at a time)

Method: `e3d6904`, `GOMAXPROCS=2`, 12-core box with the backend left running (load average 0.95-1.3; it was not restarted), one benchmark group at a time, `-benchtime 1s -count=5`, medians of the five runs. Every measurement is a probe in `/tmp/p5merge` injected with `go test -overlay`, or a benchmark already in the tree; **no file in the repository was modified**, and the two `stream_handler.go` variants (blocking drain, async drain) exist only as overlay replacements used as counterfactuals. The fixtures are local `httptest` servers: no provider, issuer or browser was contacted, so every absolute time below is a loopback number and every *ratio* is the only portable part.

| id | verdict | median, per unit | ratio / reading |
|---|---|---|---|
| PH1 (http1) | **confirmed, conditional** | 10.09 ms/round, **1.000 newconn/op** | one new upstream connection per streaming round; the connection saved is one local TLS handshake = **43 µs** (42.4-43.9 µs, 68 allocs) |
| PH1 (h2) | **does not apply** | 10.17 ms/round, 0.0085-0.0101 newconn/op | ~1 new connection per 100-117 rounds: HTTP/2 pays nothing per round (protocol verified as `HTTP/2.0`) |
| PH1 (drain counterfactual) | fix works, cost measured | 61.27 ms/round, 0.0526 newconn/op | **19.0× fewer connections**, but the round now ends when the upstream ends the stream (+51 ms here) |
| PH2 | **confirmed but small** | 132.6 µs / 606 allocs (1 turn) → 213.1 / 848 (10) → 554.3 / 1895 (50) | linear, ~9 µs and 38 allocs per turn at 20 tools; one redundant full parse is **8-9 %** (24.6 µs / 10 allocs) |
| PH2 (image body) | **confirmed** | 20 MB body: 178 ms/op, **172 MB allocated**, 80-132 MB resident after the call | 4-6.6× the request size stays on the heap |
| PH3 | **confirmed** | 10 000 text deltas: 4.83 µs and **35 allocs per delta** (1.7 KB); 10 000 tool-arg chunks: 1.40 µs, 7 allocs | the sink re-encode dominates; 2× the allocs of `openaiapi` per event, same time per event |
| PH4 | **refuted as stated** | 5 MB body: 8.02 ms (3 keys) vs 7.90 ms (52 keys); through `Stream` 19.03 vs 19.23 ms | **1.00-1.01×**: the body is *not* re-serialised per top-level key; only a plain parse is (5.45 ms) |
| PH5 | **already bounded** | 5.44-5.69 ms / 1 000 events, 53.8-54.7 ms / 10 000 | 5.4 µs and 18 allocs per event at both sizes: the adapter (4.83 µs) is in the same band |
| PH6 | **closed** | 13.1 µs, 44 allocs, 3 161 B per `Load` | same order as the Phase-0 figure in §9 (10.9 µs / 31 allocs); no new benchmark |
| PH7 | **confirmed, negligible** | 7.26 µs, 8 allocs, 1 192 B per `loadCredential` | 0.07 % of the 10 ms round it sits in; caching would buy nothing |
| PH8 | **confirmed** | shared client 112.1 µs/op, 0.0001 newconn/op; client+transport per call 198.6 µs/op, 1.000 newconn/op | **+86.6 µs, +28 KB, +1 connection per session** (not per round) |
| PH9 | **reasoned only** | — | the lock covers one issuer exchange, so the bound is the header timeout; no benchmark |

### PH1 — connections per adapter round (P5M-05)

Fixture: an `httptest` upstream that writes five `content_block_delta` events one per millisecond, then `message_stop`, then holds the body open for 50 ms (r2's shape). One iteration is one full round through `anthropicadapter.Handler` (`POST /v1/responses`, body read to EOF by the probe).

- **HTTP/1.1, today: 1.000 newconn/op, i.e. exactly one new upstream connection per round**, which is P5M-05 reproduced as an end-to-end property rather than a code reading.
- **HTTP/2 (TLS, `EnableHTTP2`, requested protocol verified `HTTP/2.0` in the handler): 0.0085-0.0101 newconn/op** — about one connection per hundred rounds. P5M-05 is an HTTP/1.1-only cost, as §3 said.
- **Counterfactual, overlay with the §12 decision 3 drain before `Close`:** 0.0526 newconn/op (19× fewer) and 61.27 ms/round. The round is longer by exactly the upstream's remaining stream time (50 ms of silence, plus the 64 KiB cap is never reached), because `io.Copy` waits for bytes that arrive later.
- **Second counterfactual, drain in a goroutine:** 10.1 ms/round (latency kept) but still 1.000 newconn/op when rounds follow each other, because the next round dials while the previous tail is still being read.
- **Price of what is saved:** one fresh TLS connection to a local `httptest` server is **43 µs** (42.4-43.9 µs, 68 allocs, 5.8 KB); a fresh transport plus a plain-TCP connection is **+87 µs** (PH8). A WAN handshake is larger and cannot be measured here.
- **The cost is conditional on the upstream, not on the adapter.** Two sweeps (3-4 rounds each, connection count from `http.StateNew`):
  - hold the empty body open after `message_stop` for 5 / 10 / 30 / 50 ms → **1 / 1 / 2 / 3** new connections;
  - trailing bytes after `message_stop` of 0 / 2 KB / 4 KB / 8 KB / 64 KB → **1 / 2 / 4 / 4 / 4**; 2 000 deltas with no tail → 1.
  So the abandoned body costs a connection when the upstream either keeps the stream open for roughly 30 ms or more past `message_stop`, or leaves more than ~4 KB unread. With the blocking drain every one of those shapes reuses a single connection (all 1).
  The threshold is empirical: the exact mechanism (client read buffer vs. the moment the terminator arrives) was not established, and is marked as unsettled rather than explained.

### PH2 — request translation (P5M-22's neighbour, r1 H5, r2 H02)

`translateRequest` makes three full passes over the body (decode into `map[string]json.RawMessage`, `json.Unmarshal` into `responsesRequest`, `json.Marshal` in `encodeRequestBody`), five when `extra_body` merges. Measured cost is linear in history size — 132.6 / 213.1 / 554.3 µs and 606 / 848 / 1895 allocs for 1 / 10 / 50 turns with 20 tools — so ~9 µs and ~38 allocs per turn, and the one arguably avoidable pass (the map decode) is **24.6 µs of a 20-turn body, 8-9 %** of the total. Nothing here is a factor-of-two claim. The large-body case is different: a 20 MB image request costs 178 ms, allocates 172 MB and leaves 80-132 MB resident, so the image path, not the duplicate parse, is the thing to watch (and `imagestore`/`imag resize` already own the peak-heap question in earlier phases).

### PH3 — upstream stream translation

10 000 `content_block_delta` text events (1.7 KB each) translate in 48.4 ms: **4.83 µs and 35 allocs per upstream delta**, with the sink writing exactly one Responses event per delta (`sinkev/upstreamev` = 1.001). 10 000 tool-argument chunks (1 MB total, no per-chunk output) cost 1.40 µs and 7 allocs each. The per-delta allocate count is the `event()` map copy plus the `json.Marshal` of the payload; it is the only place in this phase where a per-event allocation count is materially above the provider client's (PH5).

### PH4 — `rawRequestOptions` (P5M-22)

**Refuted as stated.** With a 5 MB `input`, 3 top-level keys cost 8.02 ms and 52 keys 7.90 ms (16 vs 220 allocs): `sjson.SetBytes` appends the fragment to the accumulated buffer instead of re-serialising the whole body, so key count changes allocations by ~200 and time not at all. Through the real `Stream` path the same body takes 19.03 ms with 2 keys and 19.23 ms with 50 (1.01×). One plain `json.Unmarshal` of the same body is 5.45 ms, so the provider layer as a whole is ~3.5× a single parse and independent of the key count. The "and again per retry attempt" half of P5M-22 is a linear multiple of the same figure and was **not** driven (no retry was provoked).

### PH5 — `openaiapi.Stream` per event

In-tree `BenchmarkStream`: 5.44-5.69 ms for 1 000 events and 53.8-54.7 ms for 10 000, i.e. **5.4 µs and 18 allocs per event at both sizes** (11 MB and 110 MB allocated per call). P-011/P-026 already put 85 % of that inside the SDK; this run is consistent with them, and it shows the adapter's 4.83 µs per delta is not worse than the client it wraps.

### PH6, PH7 — credential reads per request

`openaioauth.Source.Load`: 13.1 µs, 44 allocs (two JWTs parsed, file stat+read). §9 treated this as closed by the Phase-0 measurement of 10.9 µs / 31 allocs; the re-run agrees to within 20 %, so no benchmark is needed. `anthropicadapter.loadCredential`: 7.26 µs, 8 allocs — cheaper because it parses no JWT, and 0.07 % of a 10 ms round. Neither is worth caching, and caching would remove the freshness reason both exist.

### PH8 — one transport per session (r1 H6, r3 H-05)

Shared client: 112.1 µs/round, 0.0001 newconn/op. A client and transport built per call, the shape `providerHTTPClient` produces per runner: 198.6 µs/round, 1.000 newconn/op. The difference is **+86.6 µs, +28 KB and one connection**, once per session rather than once per round, plus the idle connection the transport keeps for `IdleConnTimeout`. So the hypothesis is right in shape and small in price; a 20-round session pays ~4 µs/round on loopback.

### PH9

No benchmark. `Refresh` holds the file lock across one issuer exchange; the bound is the issuer timeout, which A-4's header timeout is meant to supply. Unchanged from §9.

### What the measurements change

1. **P5M-05 keeps its severity but loses its unconditional performance sentence.** The mechanism is now measured end to end (1.000 newconn/op on HTTP/1.1), it does not exist on HTTP/2 (0.0085-0.0101 newconn/op against a real h2 upstream), and on HTTP/1.1 it only appears when the upstream lingers ≥~30 ms or leaves ≥4 KB after `message_stop`. The saved work is one connection, 43 µs locally over TLS. §3's sentence should be restated that way; the hygiene defect (abandoning a body) stands on its own.
2. **Decision 3's recommendation changes.** The four-line blocking drain does work (19× fewer connections) but it moves the end of the round to the end of the upstream stream: 10.09 → 61.27 ms in a fixture whose tail is 50 ms of silence. The asynchronous variant keeps the latency and gives the reuse up. So the trade is "wait for the upstream" versus "43 µs handshake", not "free fix"; if the drain is taken it must be justified by hygiene, and it should be bounded by the request context, not only by the 64 KiB.
3. **One hypothesis is dropped and two shrink.** PH4 as written is wrong (no per-key re-serialisation; drop it). PH2's redundant parse is 8-9 %, PH8's per-session transport is 87 µs — both real, neither worth a change on performance grounds. PH3 (35 allocs per delta) and PH2's 20 MB image path (172 MB allocated) are the two places where a change could still pay, and both belong to the adapter's translation, not to the provider edge.
4. **Nothing here is a new finding.** No defect was discovered by measuring; the counts in §1 and the severities in §2 stand.

### The one `-race` pass

`verify-go.sh --race "./internal/anthropicadapter/ ./internal/openaiauth/ ./internal/openaioauth/"` from the repository root, one run, `-race -count=3` on the three packages (the script's own shape). Results, with the log paths, are in the table at the end of this section.

### Still unmeasured

- **A WAN TLS handshake**, which is the real price of P5M-05's extra connection (43 µs is a loopback floor).
- **The real Anthropic upstream's behaviour after `message_stop`** — the single fact that decides whether P5M-05 costs anything in production, and unreachable from here.
- **PH4's retry half** (no retry driven) and **PH9** (no slow issuer).
- **P5M-04's buffering growth**: no oversized frame was pushed through `readSSE`, so the uncapped reader is still a read-only finding with no memory number.
- **Peak heap under a real GC schedule**: PH2's image figure is a two-sample `MemStats` delta, not a profile (profiles are outside this phase's rules).
- **Everything concurrent**: the single `-race` pass says the three packages are race-clean on their own tests; it says nothing about throughput under concurrent sessions, and no benchmark above is parallel.

### Commands, exit codes and probes

| command (from `backend-go/` unless noted) | exit | observed |
|---|---|---|
| `verify-go.sh --race "./internal/anthropicadapter/ ./internal/openaiauth/ ./internal/openaioauth/"` (repo root) | **0** | `gofmt 0`, `vet 0`, `test 0` (34 packages), `race 0` (`-race -count=3`: `anthropicadapter 1.780s`, `openaiauth 3.010s`, `openaioauth 1.033s`, no `DATA RACE` or `WARNING` line), `diffcheck 0` |
| `go test -overlay /tmp/p5merge/overlay_current.json ./internal/anthropicadapter/ -run '^$' -bench 'BenchmarkPH1Rounds\|BenchmarkPH1TLSHandshake' -benchtime 1s -count=5` | 0 | PH1 table rows, 43 µs handshake |
| `… ./internal/anthropicadapter/ -bench 'BenchmarkPH2\|BenchmarkPH3\|BenchmarkPH7'` | 0 | PH2, PH3, PH7 |
| `go test -overlay /tmp/p5merge/overlay_drain.json ./internal/anthropicadapter/ -bench 'BenchmarkPH1Rounds' -count=5` | 0 | blocking-drain counterfactual |
| `go test -overlay /tmp/p5merge/overlay_async.json ./internal/anthropicadapter/ -bench 'BenchmarkPH1Rounds/http1' -count=5` | 0 | async-drain counterfactual |
| `go test -overlay /tmp/p5merge/overlay_debug.json ./internal/anthropicadapter/ -run 'TestPH1States\|TestPH1Variants\|TestPH1H2Proto'` | 0 | the two sweeps and the `HTTP/2.0` protocol check |
| `go test -overlay /tmp/p5merge/overlay_current.json ./internal/openaiapi/ -bench 'BenchmarkPH4\|BenchmarkPH8\|BenchmarkStream$' -count=5` | 0 | PH4, PH8, PH5 |
| `go test -overlay /tmp/p5merge/overlay_current.json ./internal/openaioauth/ -bench 'BenchmarkPH6' -count=5` | 0 | PH6 |
| `git rev-parse --short HEAD`, `git status --short`, `git diff --check` | 0 | `e3d6904`; status identical to §10; no whitespace error |

Probes, all under `/tmp/p5merge`: `adapter_p5_measure_test.go` (PH1, PH2, PH3, PH7, TLS handshake), `openaiapi_p5_measure_test.go` (PH4, PH8), `openaioauth_p5_measure_test.go` (PH6), `ph1_debug_test.go`, `ph1_probe2_test.go`, `ph1_states_test.go`, `ph1_h2_test.go` (sweeps and controls), and the two overlay-only counterfactuals `stream_handler_drain.go`, `stream_handler_async.go`. Removed at the end of this step.

One correction to my own harness, recorded because it produced a wrong number first: the initial PH1 benchmark built the TLS shape with `server.Start()` instead of `server.StartTLS()`, so its "h2" column was silently another HTTP/1.1 run. The control test that prints the negotiated protocol caught it, the fixture was fixed, and the h2 row above is from the corrected run. Any future probe of this kind should assert `r.Proto` rather than trust the label.
