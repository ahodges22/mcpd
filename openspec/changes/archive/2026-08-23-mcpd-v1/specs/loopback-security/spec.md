## ADDED Requirements

### Requirement: Loopback-only binding

The daemon SHALL bind only to the loopback interface and SHALL NOT bind all interfaces.

#### Scenario: The listener is not externally reachable

- **WHEN** the daemon starts
- **THEN** it listens on the loopback address only

### Requirement: DNS-rebinding protection is enabled and verified

Cross-origin protection SHALL be active on every route. Because the daemon can invoke
destructive tools and change backend state, a remote page in an open browser tab must not
be able to reach it, and DNS rebinding defeats a naive same-origin assumption.

Protection SHALL NOT be disabled for convenience, and its being active SHALL be asserted
by a test rather than assumed from a library default, because an unguarded daemon is
indistinguishable from a guarded one until it matters.

#### Scenario: A foreign origin is rejected on an MCP endpoint

- **WHEN** a request carrying a cross-site origin reaches either MCP endpoint
- **THEN** it is rejected

#### Scenario: A foreign origin is rejected on the web routes

- **WHEN** a request carrying a cross-site origin reaches the status API
- **THEN** it is rejected, because the web routes are served by different code from the
  MCP endpoints and are not covered by the MCP handler's own protection

#### Scenario: A request with no origin is accepted

- **WHEN** a non-browser MCP client sends a request with no origin header
- **THEN** it is accepted, because rejecting it would break every native client

### Requirement: One protection policy shared across surfaces

The MCP endpoints and the web routes SHALL enforce cross-origin policy from a single
shared value rather than from two independently constructed configurations, because two
configurations drift and the drift is invisible until a route that should have been
protected is not.

#### Scenario: Policy cannot diverge between surfaces

- **WHEN** the cross-origin policy is changed
- **THEN** both the MCP endpoints and the web routes observe the change, because they hold
  the same value

### Requirement: State changes require POST and JSON

Every state-changing route SHALL require a POST with a JSON content type, so that a simple
cross-origin form submission cannot trigger one and no state change is reachable by
navigation or image loading.

The OAuth callback SHALL be the single exemption, being necessarily a top-level browser
GET, and SHALL be protected instead by its one-time `state` nonce.

#### Scenario: A mutation is not reachable by GET

- **WHEN** a state-changing route is requested with GET
- **THEN** it is rejected

#### Scenario: Only the callback may change state on GET

- **WHEN** the set of routes that change state on GET is enumerated
- **THEN** it contains the OAuth callback and nothing else

### Requirement: Backend-derived output is escaped

Tool names, descriptions, error text, and tool call results all originate from
third-party servers, including packages fetched from a public registry at run time. All of
it SHALL be rendered as text rather than interpolated into markup.

Tool results are the most dangerous of these, because a result is the one field a user
expects to be rich: an unescaped result yields same-origin script execution, and
same-origin script can drive the very mutation routes the origin checks protect, bypassing
them entirely.

#### Scenario: A malicious tool result is inert

- **WHEN** a tool returns a result containing markup and a script payload
- **THEN** it is displayed as literal text and no element is created from it

#### Scenario: A malicious tool description is inert

- **WHEN** a backend advertises a tool whose description contains markup
- **THEN** the status surface displays it as literal text

### Requirement: The trust boundary is stated rather than implied

The daemon has no user authentication, so any process on the machine can reach it. This
SHALL be documented rather than left implicit, and SHALL NOT be described as equivalent to
the pre-existing situation: introducing an HTTP surface where none existed is a real
increase in exposure, and the mitigations above exist because of it.

A hostile stdio backend SHALL NOT be described as contained. It runs as the same user and
can read the token store and every other credential file, so the least-privilege child
environment addresses accidental grants and auditability, not containment.

Declaring a stdio backend starts a process, so the add-backend route is the
highest-privilege operation on the surface and SHALL be documented as such. It does not
widen the boundary, because a local process could already write the declaration file and
wait for a restart, but it removes the wait. The origin, method and loopback-host guards are
what keep a browser page from reaching it. They do not constrain another local process, and
nothing here does.

#### Scenario: The documented threat model matches the implementation

- **WHEN** the security posture is described in project documentation
- **THEN** it states that local processes are trusted, that a hostile stdio backend is not
  contained, that the add-backend route can start a process, and which specific mitigations
  apply to browser-originated requests

#### Scenario: The process-spawning route is subject to every guard

- **WHEN** the add-backend route is reached with a cross-site origin, with GET, or with a
  non-loopback host header
- **THEN** it is rejected, because a route that starts a process is the last one that may
  be reachable from a browser page
