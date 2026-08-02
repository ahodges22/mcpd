# Security policy

## Trust boundary

mcpd is a single-user, loopback-only daemon. It listens on `127.0.0.1` and
performs no user authentication: any process running as the same user can call
every connected tool. It is not a multi-user MCP gateway, and its listener must
not be exposed on a network interface. See the "Security model" section of the
[README](README.md) for the full model.

## Reporting a vulnerability

Report suspected vulnerabilities privately through GitHub's
[private vulnerability reporting](https://github.com/ahodges22/mcpd/security/advisories/new).
Please do not open a public issue for a security report.

Include the affected version or commit, a description of the issue, and a
reproduction if you have one. You can expect an initial response within a few
days.
