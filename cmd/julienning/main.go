// Command julienning manages shared Claude Code accounts: config dirs, shell
// aliases, session hand-off, and usage tracking through a Cloudflare Worker.
package main

import (
	"os"
	_ "time/tzdata" // a forwarded command renders the controller's zone on hosts without zoneinfo

	"github.com/muratgozel/julienning/internal/cli"
	_ "github.com/muratgozel/julienning/internal/cli/accounts"  // registers commands
	_ "github.com/muratgozel/julienning/internal/cli/dirs"      // registers commands
	_ "github.com/muratgozel/julienning/internal/cli/hosts"     // registers commands and --host forwarding
	_ "github.com/muratgozel/julienning/internal/cli/maint"     // registers commands
	_ "github.com/muratgozel/julienning/internal/cli/switching" // registers commands
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
