// Command mcpd is the standalone mcpd CLI.
package main

import (
	"os"

	"github.com/ahodges22/mcpd/mcpdcmd"
)

func main() {
	os.Exit(mcpdcmd.Main(os.Args[1:]))
}
