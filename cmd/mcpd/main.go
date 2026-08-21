// Command mcpd is the local MCP proxy daemon. It multiplexes several upstream MCP
// servers behind one loopback endpoint and serves a status surface. The whole CLI
// lives in mcpdcmd so that multi-tool binaries can embed it; this command is the
// standalone shell around it.
package main

import (
	"os"

	"github.com/ahodges22/mcpd/mcpdcmd"
)

func main() {
	os.Exit(mcpdcmd.Main(os.Args[1:]))
}
