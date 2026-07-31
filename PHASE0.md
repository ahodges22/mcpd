# Phase 0: Notion OAuth feasibility gate

Run 2026-07-28 via `cmd/oauthprobe`, against real `https://mcp.notion.com/mcp`.
Gate defined in the design spec, section "Live OAuth acceptance against Notion".

## Verdict

**Row 1 of the gate table: discovery, DCR, and a loopback redirect all work.
Proceed as designed.** Task 10 uses `DynamicClientRegistrationConfig`. No design
change required, and the fixed-port assumption holds.

## Findings

| question | result |
|---|---|
| Q1a: does an unauthenticated request yield a discoverable challenge? | **Yes.** `401` with `WWW-Authenticate: Bearer realm="OAuth", resource_metadata="https://mcp.notion.com/.well-known/oauth-protected-resource/mcp"` |
| Q1b: protected-resource metadata present and self-consistent? | **Yes.** `resource` matches the server URL, so the SDK's equality check passes. `authorization_servers: ["https://mcp.notion.com"]` |
| Q2: authorization-server metadata, and does it advertise DCR? | **Yes.** `registration_endpoint: https://mcp.notion.com/register`. PKCE `S256` supported, so the SDK's PKCE assertion passes. `grant_types_supported` includes `refresh_token` |
| Q3: does DCR accept a plain-HTTP loopback redirect URI? | **Yes.** `HTTP 201`, `redirect_uris` echoed back verbatim as `http://127.0.0.1:7420/oauth/callback` |
| Q4: is a refresh token actually issued? | **Deferred to Task 14.** Needs an interactive grant. `refresh_token` appears in `grant_types_supported`, which is necessary but not sufficient |

## Details worth carrying into Task 10

- **Public client, no secret to store.** `token_endpoint_auth_method: none` is
  both supported and what registration returned. `internal/oauthstore` still needs
  to persist the `client_id`, because a re-registration on every restart would be
  wasteful and would churn clients at Notion, but there is no client secret to
  protect. The store's 0600 mode is still required for the access and refresh
  tokens.
- **Discovery must start from the 401 challenge, not a guessed well-known path.**
  Notion's resource metadata lives at
  `/.well-known/oauth-protected-resource/mcp`, with the `/mcp` path suffix. A
  probe that guessed `/.well-known/oauth-protected-resource` without the suffix
  would have to be right about a detail the challenge simply tells you.
- **`code_challenge_methods_supported` includes `plain`.** The SDK uses `S256`;
  nothing to do, but worth knowing the server would accept a weaker method if
  something else in the chain chose it.
- **Endpoints:** authorize `https://mcp.notion.com/authorize`, token
  `https://mcp.notion.com/token`, register `https://mcp.notion.com/register`.

## Corrections this probe forced on the implementation plan

The plan's Task 1 code was written from inferred signatures and did not compile.
Actual `oauthex` API, confirmed with `go doc`:

```go
func GetProtectedResourceMetadata(ctx context.Context, metadataURL, resourceURL string, c *http.Client) (*ProtectedResourceMetadata, error)
func GetAuthServerMeta(ctx context.Context, metadataURL, issuer string, c *http.Client) (*AuthServerMeta, error)
func RegisterClient(ctx context.Context, registrationEndpoint string, clientMeta *ClientRegistrationMetadata, c *http.Client) (*ClientRegistrationResponse, error)
func ParseWWWAuthenticate(headers []string) ([]Challenge, error)
```

Three differences from the plan's draft: the `*http.Client` is the **last**
parameter rather than the second; both metadata getters take a `metadataURL`
**and** the resource/issuer URL, because they verify the two agree; and the
functions are documented under their return types, so `go doc <pkg> | grep '^func '`
does not list them. The plan has been corrected.

This is the value of the gate: the same class of mistake in Task 10, discovered
after the daemon was built around it, would have been considerably more expensive.

## Housekeeping

Two throwaway client registrations were created at Notion during probing:
`sg-incGl2s9SHpeL` (manual curl) and `0ap1tkap02JT7AHB` (via the SDK path). Both
are unused public clients with no secret and no granted authorization. They are
inert, but if Notion's UI exposes registered-client management they can be removed.
Task 10 will register its own client and persist that one.

## Task 14: live provider acceptance, 2026-07-31

Both OAuth backends authorize, connect and serve. Every declared backend on this
machine is up at once for the first time: 14 of 14, 611 tools.

| step | notion | metabase |
|---|---|---|
| needs-auth with a pending URL | yes | yes |
| consent completed in the browser | yes, 20 tools | yes, 8 tools |
| real authenticated call | `notion-get-users`, 11.7KB | tools listed |
| token reused across restart | yes, no re-authorization | yes |
| forced expiry refreshes | yes, 16:04 to 16:31, stayed up | not forced |

Notion has never worked through a proxy before, and `metabase` "has never
authenticated anywhere": the proposal listed it under **out of scope** for that
reason. It is in scope now, because the two defects that kept it out were found and
fixed here.

### Both were upstream defects, and both cost the whole backend

**Metabase's 401 challenge omits a comma.** It answers with
`Bearer realm="mcp" resource_metadata="..."`, with no comma between the auth-params.
RFC 9110 section 11.2 requires them to be comma-separated, so `ParseWWWAuthenticate`
refuses it and discovery never starts. The transport repairs the header on the way in,
inserting only the comma the spec requires; a well-formed header already has it, so
the repair is a no-op everywhere else, and the repaired header goes through the same
parser and the same checks.

**Metabase returns an `iss` parameter it never advertises.** RFC 9207 makes `iss`
conditional on the authorization server declaring
`authorization_response_iss_parameter_supported`, and the SDK enforces that in both
directions. Metabase's metadata omits the flag and its redirect includes `iss`, so a
consent the user had already given was discarded before any token exchange, and the
flow silently restarted. mcpd now reads the same flag when it publishes an
authorization: an `iss` that was never advertised is checked here, against the origin
the user was actually sent to, and then not forwarded. A **mismatched** `iss` is
refused at the callback and the code is never delivered, which is stricter than
before. An advertised `iss` is still forwarded untouched.

Notion is unaffected by either: it sends no `iss` at all, which is the consistent
case, and it is why an unconditional fix would have been wrong.

### The finding that made the difference

This took three rounds to diagnose, and the reason is worth recording: **the
authorization flow logged nothing at all.** A flow with a dozen failure modes left no
trace, and the health record holds only the *latest* failure, so the fast retry after
each rejection overwrote the error that explained it. Every visible symptom was the
second attempt's `context canceled`.

The flow now logs the callback's `iss`, the provider's own refusals, the budget each
wait is given, every way a wait can end, and every handshake failure with its stage
and cause. Never the URL, the state nonce, the code, or any token. Two real bugs were
found and fixed along the way that were not the cause of this one:

- An abandoned authorize request destroyed the authorization it had started. The
  wait's context was the HTTP request's, so navigating the panel to the consent screen
  in the same tab cancelled it, and the route read that as evidence the stored grant
  was unusable. Cancellation and deadline expiry are now distinguished.
- The window a person had to complete a consent was bounded by a machine handshake
  budget, 60 seconds by default. A handshake the user asks for now gets a budget sized
  for a person, and the consent screen opens in its own tab so the panel is never
  navigated away from.
