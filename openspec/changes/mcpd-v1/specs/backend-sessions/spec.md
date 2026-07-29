## ADDED Requirements

### Requirement: Single shared session per backend

The daemon SHALL hold at most one upstream session per backend and multiplex all
connected clients onto it. A stdio backend SHALL be spawned once for the daemon's
lifetime, not once per connected client.

#### Scenario: Two clients share one stdio child

- **WHEN** two MCP clients are connected and both call a tool on the same stdio backend
- **THEN** exactly one child process exists for that backend

#### Scenario: Server-initiated flows are not forwarded

- **WHEN** a backend issues a sampling, elicitation, or roots request
- **THEN** the daemon does not forward it to any client, because one shared session
  cannot attribute it to one of several clients

### Requirement: At-most-once tool dispatch

A `tools/call` SHALL be delivered at most once. The daemon SHALL reconnect and send
only when no send was attempted for that call. Once a send has begun, the daemon SHALL
NOT replay the call under any error, including an error returned by the write itself,
because a failed write is not evidence that the upstream did not receive and act on the
request.

#### Scenario: No session yet, so a retry is safe

- **WHEN** a tool call finds no connected session, or connection establishment fails
- **THEN** the daemon reconnects and sends the call once, and reports the failure as
  retryable

#### Scenario: The response is lost after the upstream acted

- **WHEN** a backend commits a side effect and then drops the connection before
  responding
- **THEN** exactly one side effect has occurred, and the error states that the outcome
  is unknown and names the tool

#### Scenario: The write reports an error after delivering

- **WHEN** the transport reports a write error for a request the backend has already
  received and acted on
- **THEN** the daemon does not replay the call, so exactly one side effect has occurred

### Requirement: Least-privilege stdio child environment

The daemon SHALL construct each stdio child's environment explicitly and SHALL NOT
inherit its own environment wholesale. The child environment SHALL consist of a curated
base of variables any process needs, plus that backend's declared `env`, plus that
backend's declared `env_passthrough` patterns, and nothing else.

#### Scenario: A declared credential is granted

- **WHEN** a backend declares `env_passthrough` of `["AWS_*", "KUBECONFIG"]`
- **THEN** its child receives the matching variables from the daemon's environment,
  along with `PATH` and `HOME`

#### Scenario: An undeclared credential is withheld

- **WHEN** a backend declares no `env` and no `env_passthrough`
- **THEN** its child receives only the curated base, and none of the daemon's
  credentials such as `GH_PAT`, `DD_ACCESS_TOKEN`, or `LITELLM_KEY`

#### Scenario: The environment is never left unset

- **WHEN** any stdio child is spawned
- **THEN** its environment is an explicitly constructed value, never the unset value
  that would cause the child to inherit everything the daemon holds

### Requirement: Backend health is observable

Each backend SHALL expose a health record carrying its state, transport, tool count,
last refresh time, last error, and authentication note. A backend that fails SHALL be
recorded with its error rather than silently omitted.

#### Scenario: An unreachable backend reports why

- **WHEN** a backend's host cannot be resolved
- **THEN** its health record shows a down state and retains the resolution error text

### Requirement: Serialized lifecycle transitions

Connect, disconnect, enable, disable, and reconnect for one backend SHALL be
serialized. Each backend SHALL carry a generation counter incremented on every
lifecycle transition, and any in-flight operation whose generation has changed SHALL
discard its result rather than commit it.

#### Scenario: A late refresh cannot resurrect a disabled backend

- **WHEN** a `tools/list` is in flight and the backend is disabled before it returns
- **THEN** the refresh result is discarded and the disabled backend's tools do not
  appear in the catalog

#### Scenario: A pending retry cannot respawn a disabled backend

- **WHEN** a backoff retry is scheduled and the backend is disabled before it fires
- **THEN** the retry is cancelled and awaited, and no child process is respawned

### Requirement: Disable is a kill switch

Disabling a backend SHALL close its session, terminate its stdio child if it has one,
and remove its tools from every endpoint's catalog. The override SHALL be persisted
before teardown begins, so that a crash mid-disable leaves the backend disabled rather
than silently re-enabled.

#### Scenario: Disable removes tools and stops the process

- **WHEN** a backend is disabled from the status API
- **THEN** its session is closed, its child process is terminated, and its tools are
  absent from both endpoints

#### Scenario: The override outlives a restart

- **WHEN** a backend is disabled and the daemon is restarted
- **THEN** the backend remains disabled and is not connected on startup

#### Scenario: A dispatch cannot outrun a disable

- **WHEN** a tool call has passed its enabled check but not yet written its request, and
  a disable runs concurrently
- **THEN** the call either completes or is rejected, and in no case is a request written
  to the transport after the gate has closed
