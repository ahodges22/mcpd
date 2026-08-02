package mcpsrv

import (
	"sort"
	"strings"

	"github.com/ahodges22/mcpd/internal/backend"
)

// Instructions are what a client puts in front of the model to say what a server is for. Without
// them the model has only tool names to infer the point of the server from, and for the facade
// there is nothing to infer: three generic verbs do not suggest that several hundred tools sit
// behind them. The prototype mcpd replaced carried this text and mcpd did not, which is how an
// agent could have every tool listed and still never reach for one.
//
// The backend list is a snapshot from when the server was built, because that is when the SDK
// takes the string. A backend declared later is still reachable and still appears in tools/list;
// it is only missing from this orientation text until the daemon restarts.
func searchInstructions(reg *backend.Registry) string {
	var b strings.Builder
	b.WriteString("Every tool from every connected MCP backend is reachable through this server, " +
		"but none of them is preloaded: it advertises three tools rather than several hundred.\n\n" +
		"Call search_tools with a plain description of the task to find candidates, describe_tool " +
		"to read one tool's full input schema, then call_tool with that tool's canonical id and " +
		"its arguments. Results flagged low_confidence may not answer the query, so rephrase " +
		"instead of calling one on the assumption that it fits.\n")
	writeBackends(&b, reg)
	return b.String()
}

func passthroughInstructions(reg *backend.Registry) string {
	var b strings.Builder
	b.WriteString("This server proxies every tool from every connected MCP backend, each named " +
		"mcp__<backend>__<tool>. Those backends are reached only through here, so search or list " +
		"the tools on this server rather than expecting them to be configured separately. A tool " +
		"that is absent means its backend is not connected at the moment, not that it does not " +
		"exist.\n")
	writeBackends(&b, reg)
	return b.String()
}

// writeBackends names what is declared. Knowing that a domain is present at all is what tells a
// model there is something here worth searching for.
func writeBackends(b *strings.Builder, reg *backend.Registry) {
	names := reg.Names()
	if len(names) == 0 {
		return
	}
	sort.Strings(names)
	b.WriteString("\nBackends declared when this server started: " + strings.Join(names, ", ") + ".\n")
}
