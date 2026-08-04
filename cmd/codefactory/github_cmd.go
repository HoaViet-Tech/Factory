package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/HoaViet-Tech/factory/internal/api"
	"github.com/HoaViet-Tech/factory/internal/client"
	"github.com/HoaViet-Tech/factory/internal/githubcli"
	"github.com/HoaViet-Tech/factory/internal/labels"
)

const githubUsage = `GitHub ingest helpers.

Usage:
  codefactory github poll [--server URL]
  codefactory github labels --repo owner/name [--dry-run]

"poll" asks the server to scan configured repositories once, right now.
"labels" creates the factory:* labels in a repository so the workflow can start.
`

func runGitHub(args []string) error {
	if len(args) == 0 {
		fmt.Print(githubUsage)
		return nil
	}

	switch args[0] {
	case "poll":
		return githubPoll(args[1:])
	case "labels":
		return githubLabels(args[1:])
	case "help", "--help", "-h":
		fmt.Print(githubUsage)
		return nil
	default:
		return fmt.Errorf("unknown github subcommand %q\n\n%s", args[0], githubUsage)
	}
}

func githubPoll(args []string) error {
	fs := flag.NewFlagSet("github poll", flag.ExitOnError)
	serverURL := fs.String("server", defaultServer, "control plane URL")
	if err := fs.Parse(args); err != nil {
		return err
	}

	resp, err := client.New(*serverURL).Poll()
	if err != nil {
		return err
	}

	fmt.Printf("polled %d repositor%s\n", resp.Repositories, plural(resp.Repositories, "y", "ies"))
	fmt.Printf("  issues seen:    %d\n", resp.IssuesSeen)
	fmt.Printf("  tasks created:  %d\n", resp.TasksCreated)
	fmt.Printf("  already known:  %d\n", resp.Skipped)
	if resp.DryRun {
		fmt.Println("  (server is in GitHub dry-run mode: no writes will happen)")
	}
	for _, id := range resp.CreatedTaskIDs {
		fmt.Printf("  created task %s\n", id)
	}
	for _, e := range resp.Errors {
		fmt.Fprintf(os.Stderr, "  error: %s\n", e)
	}
	return nil
}

// githubLabels bootstraps the label vocabulary in a repository. Without the
// labels there is nothing for the poller to find, so this is step one of the
// GitHub demo.
func githubLabels(args []string) error {
	fs := flag.NewFlagSet("github labels", flag.ExitOnError)
	repo := fs.String("repo", "", "repository in owner/name form (required)")
	dryRun := fs.Bool("dry-run", false, "print what would be created without creating it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *repo == "" {
		return fmt.Errorf("--repo owner/name is required")
	}
	owner, name, err := api.ParseFullName(*repo)
	if err != nil {
		return err
	}
	full := owner + "/" + name

	gh := githubcli.New(*dryRun)
	gh.Logf = log.New(os.Stdout, "", 0).Printf
	if err := gh.Available(); err != nil {
		return err
	}

	colors := map[string]string{
		labels.Inbox:      "0e8a16",
		labels.Refining:   "fbca04",
		labels.NeedsHuman: "d93f0b",
		labels.Ready:      "1d76db",
		labels.Active:     "5319e7",
		labels.Review:     "006b75",
		labels.Blocked:    "b60205",
		labels.Done:       "c2e0c6",
	}

	for _, l := range labels.All() {
		if err := gh.EnsureLabel(full, l, labels.Descriptions[l], colors[l]); err != nil {
			return fmt.Errorf("create label %s: %w", l, err)
		}
		fmt.Printf("ensured label %s\n", l)
	}
	fmt.Printf("\nNext: label an issue %s and run `codefactory github poll`\n", labels.Inbox)
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
