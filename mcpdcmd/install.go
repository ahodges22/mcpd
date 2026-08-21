package mcpdcmd

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/ahodges22/mcpd/internal/install"
)

// runInstall points clients at mcpd, or takes them back off it.
//
// It defaults to reporting rather than acting. The files it edits are the highest-risk
// surface in this whole change, because a bug here damages live configuration rather than the
// proxy, so seeing the plan is the default and applying it is the flag.
func runInstall(args []string) error {
	fs := flag.NewFlagSet("mcpd install", flag.ContinueOnError)
	var (
		client = fs.String("client", "", "client to rewire: claude, codex, cursor, opencode, or all")
		addr   = fs.String("addr", "127.0.0.1:7420", "address mcpd serves on")
		apply  = fs.Bool("apply", false, "make the changes; without it, only report them")
		revert = fs.Bool("revert", false, "remove what mcpd added, keeping everything else")
		state  = fs.String("state", defaultPath("XDG_STATE_HOME", ".local/state", ""), "state directory")
	)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: mcpd install --client <name> [--apply] [--revert]")
		fmt.Fprintln(fs.Output(), "\nReports what it would change unless --apply is given.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *client == "" {
		fs.Usage()
		return errors.New("--client is required")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("find the home directory: %w", err)
	}
	targets, err := targets(home, *client)
	if err != nil {
		return err
	}

	var failed []string
	for _, c := range targets {
		if err := one(c, *state, *addr, *apply, *revert); err != nil {
			fmt.Printf("%s: %v\n\n", c.Name, err)
			failed = append(failed, c.Name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%s: not changed", strings.Join(failed, ", "))
	}
	if !*apply {
		fmt.Println("Nothing was changed. Add --apply to make these changes.")
	}
	return nil
}

func targets(home, client string) ([]install.Client, error) {
	if client == "all" {
		return install.Clients(home), nil
	}
	c, err := install.Lookup(home, client)
	if err != nil {
		return nil, err
	}
	return []install.Client{c}, nil
}

func one(c install.Client, state, addr string, apply, revert bool) error {
	plan, err := planFor(c, state, addr, revert)
	if err != nil {
		return err
	}
	verb := "install"
	if revert {
		verb = "revert"
	}
	fmt.Printf("%s %s (%s)\n", verb, c.Name, c.Path)
	if plan.Empty() {
		fmt.Println("  nothing to do")
		return nil
	}
	for _, note := range plan.Notes {
		fmt.Println("  - " + note)
	}
	for _, warning := range plan.Warnings {
		fmt.Println("  ! " + warning)
	}
	// The facade collapses every upstream tool behind one call_tool, so a client served it
	// has one approval decision for all of them rather than one per tool. Said here because
	// the alternative is the user discovering it when a prompt they expected does not appear.
	if !revert && c.Mode == install.Search {
		fmt.Println("  ! this client is served the facade, so per-tool approval settings no longer apply:")
		fmt.Println("    it sees one call_tool and therefore one approval decision for every upstream tool")
	}
	if !apply {
		return nil
	}
	if revert {
		if err := c.Revert(state, plan); err != nil {
			return err
		}
	} else if err := c.Apply(state, plan); err != nil {
		return err
	}
	fmt.Println("  done. Restart " + c.Name + " to pick it up.")
	return nil
}

func planFor(c install.Client, state, addr string, revert bool) (install.Plan, error) {
	if revert {
		return c.PlanRevert(state)
	}
	return c.PlanInstall(addr)
}
