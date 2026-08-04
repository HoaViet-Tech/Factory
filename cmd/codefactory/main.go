// Command codefactory is the single binary for the whole system: it runs the
// control plane server, runs workers, and acts as the CLI client.
//
// One binary keeps the demo short — you only ever build one thing — while the
// subcommands stay strictly separated internally.
package main

import (
	"fmt"
	"os"
	"strings"
)

const usage = `codefactory — a local-first control plane for AI coding agents

Usage:
  codefactory <command> [flags]

Commands:
  server        Run the control plane (HTTP API + SQLite + background loops)
  worker        Run a worker that claims tasks and executes them in git worktrees
  repo          Manage repositories the factory is allowed to work on
  task          Create and inspect tasks
  github        GitHub ingest helpers (poll, label bootstrap)
  version       Print the version

Run "codefactory <command> --help" for the flags of a command.

Quick start (no GitHub credentials needed):
  codefactory server --db ./factory.db --listen 127.0.0.1:7337
  codefactory repo add local/demo --clone-url /path/to/a/local/repo
  codefactory worker --server http://127.0.0.1:7337 --name local-fake --runtime fake
  codefactory task create --repo local/demo --title "Try the factory" --prompt "Say hello"
  codefactory task list
`

const version = "0.1.0"

// splitPositional pulls a leading positional argument out of args.
//
// The standard flag package stops parsing at the first non-flag argument, so
// without this `repo add owner/name --clone-url X` would silently drop the
// flag. Lifting the operand out first lets flags appear on either side of it.
func splitPositional(args []string) (positional string, rest []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "server":
		err = runServer(args)
	case "worker":
		err = runWorker(args)
	case "repo":
		err = runRepo(args)
	case "task":
		err = runTask(args)
	case "github":
		err = runGitHub(args)
	case "version", "--version", "-v":
		fmt.Printf("codefactory %s\n", version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
