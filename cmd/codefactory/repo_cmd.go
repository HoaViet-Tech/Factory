package main

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/HoaViet-Tech/factory/internal/api"
	"github.com/HoaViet-Tech/factory/internal/client"
)

const repoUsage = `Manage repositories the factory is allowed to work on.

Usage:
  codefactory repo add owner/name [--clone-url URL]
  codefactory repo list

The clone URL may be a local path, which is how the credential-free demo works.
`

func runRepo(args []string) error {
	if len(args) == 0 {
		fmt.Print(repoUsage)
		return nil
	}

	switch args[0] {
	case "add":
		return repoAdd(args[1:])
	case "list", "ls":
		return repoList(args[1:])
	case "help", "--help", "-h":
		fmt.Print(repoUsage)
		return nil
	default:
		return fmt.Errorf("unknown repo subcommand %q\n\n%s", args[0], repoUsage)
	}
}

func repoAdd(args []string) error {
	fs := flag.NewFlagSet("repo add", flag.ExitOnError)
	serverURL := fs.String("server", defaultServer, "control plane URL")
	cloneURL := fs.String("clone-url", "", "clone URL or local path (default: https://github.com/owner/name.git)")
	disabled := fs.Bool("disabled", false, "register the repository but exclude it from polling")

	target, rest := splitPositional(args)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if target == "" {
		target = fs.Arg(0)
	}
	if target == "" {
		return fmt.Errorf("usage: codefactory repo add owner/name [--clone-url URL]")
	}

	owner, name, err := api.ParseFullName(target)
	if err != nil {
		return err
	}

	enabled := !*disabled
	repo, err := client.New(*serverURL).AddRepository(api.CreateRepositoryRequest{
		Owner:    owner,
		Name:     name,
		CloneURL: *cloneURL,
		Enabled:  &enabled,
	})
	if err != nil {
		return err
	}

	fmt.Printf("registered %s\n", repo.FullName())
	fmt.Printf("  clone url:  %s\n", repo.CloneURL)
	fmt.Printf("  cache path: <work-dir>/%s\n", repo.LocalCachePath)
	fmt.Printf("  enabled:    %v\n", repo.Enabled)
	return nil
}

func repoList(args []string) error {
	fs := flag.NewFlagSet("repo list", flag.ExitOnError)
	serverURL := fs.String("server", defaultServer, "control plane URL")
	if err := fs.Parse(args); err != nil {
		return err
	}

	repos, err := client.New(*serverURL).ListRepositories()
	if err != nil {
		return err
	}
	if len(repos) == 0 {
		fmt.Println("no repositories registered; add one with `codefactory repo add owner/name`")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REPOSITORY\tENABLED\tCLONE URL")
	for _, r := range repos {
		fmt.Fprintf(tw, "%s\t%v\t%s\n", r.FullName(), r.Enabled, r.CloneURL)
	}
	return tw.Flush()
}
