package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/HoaViet-Tech/factory/internal/api"
	"github.com/HoaViet-Tech/factory/internal/client"
)

const defaultServer = "http://127.0.0.1:7337"

const taskUsage = `Create and inspect tasks.

Usage:
  codefactory task create --repo owner/name --title "..." --prompt "..."
  codefactory task list [--status queued] [--kind manual] [--repo owner/name]
  codefactory task show TASK_ID
  codefactory task cancel TASK_ID
`

func runTask(args []string) error {
	if len(args) == 0 {
		fmt.Print(taskUsage)
		return nil
	}

	switch args[0] {
	case "create":
		return taskCreate(args[1:])
	case "list", "ls":
		return taskList(args[1:])
	case "show":
		return taskShow(args[1:])
	case "cancel":
		return taskCancel(args[1:])
	case "help", "--help", "-h":
		fmt.Print(taskUsage)
		return nil
	default:
		return fmt.Errorf("unknown task subcommand %q\n\n%s", args[0], taskUsage)
	}
}

func taskCreate(args []string) error {
	fs := flag.NewFlagSet("task create", flag.ExitOnError)
	serverURL := fs.String("server", defaultServer, "control plane URL")
	repo := fs.String("repo", "", "repository in owner/name form (required)")
	title := fs.String("title", "", "short task title (required)")
	promptText := fs.String("prompt", "", "prompt text for the agent")
	promptFile := fs.String("prompt-file", "", "read the prompt from a file instead of --prompt")
	kind := fs.String("kind", api.KindManual, "task kind: manual, refine_ticket, implement_ticket, review_pr")
	issue := fs.Int("issue", 0, "GitHub issue number this task relates to (optional)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *repo == "" || *title == "" {
		fs.Usage()
		return fmt.Errorf("--repo and --title are required")
	}
	owner, name, err := api.ParseFullName(*repo)
	if err != nil {
		return err
	}

	prompt := *promptText
	if *promptFile != "" {
		data, err := os.ReadFile(*promptFile)
		if err != nil {
			return fmt.Errorf("read prompt file: %w", err)
		}
		prompt = string(data)
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = *title
	}

	req := api.CreateTaskRequest{
		Kind:      *kind,
		RepoOwner: owner,
		RepoName:  name,
		Title:     *title,
		Prompt:    prompt,
	}
	if *issue > 0 {
		req.GitHubIssueNumber = issue
	}

	task, err := client.New(*serverURL).CreateTask(req)
	if err != nil {
		return err
	}
	fmt.Printf("created task %s (%s) for %s\n", task.ID, task.Kind, task.FullName())
	fmt.Printf("watch it with: codefactory task show %s\n", task.ID)
	return nil
}

func taskList(args []string) error {
	fs := flag.NewFlagSet("task list", flag.ExitOnError)
	serverURL := fs.String("server", defaultServer, "control plane URL")
	status := fs.String("status", "", "filter by status")
	kind := fs.String("kind", "", "filter by kind")
	repo := fs.String("repo", "", "filter by repository (owner/name)")
	limit := fs.Int("limit", 50, "maximum rows")
	if err := fs.Parse(args); err != nil {
		return err
	}

	tasks, err := client.New(*serverURL).ListTasks(*status, *kind, *repo, *limit)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		fmt.Println("no tasks yet")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tKIND\tSTATUS\tREPO\tISSUE\tAGE\tTITLE")
	for _, t := range tasks {
		issue := "-"
		if t.GitHubIssueNumber != nil {
			issue = fmt.Sprintf("#%d", *t.GitHubIssueNumber)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			t.ID, t.Kind, t.Status, t.FullName(), issue,
			age(t.CreatedAt), truncate(t.Title, 50))
	}
	return tw.Flush()
}

func taskShow(args []string) error {
	fs := flag.NewFlagSet("task show", flag.ExitOnError)
	serverURL := fs.String("server", defaultServer, "control plane URL")
	showPrompt := fs.Bool("prompt", false, "also print the full prompt")

	taskID, rest := splitPositional(args)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if taskID == "" {
		taskID = fs.Arg(0)
	}
	if taskID == "" {
		return fmt.Errorf("usage: codefactory task show TASK_ID")
	}

	c := client.New(*serverURL)
	task, err := c.GetTask(taskID)
	if err != nil {
		return err
	}

	fmt.Printf("Task    %s\n", task.ID)
	fmt.Printf("Kind    %s\n", task.Kind)
	fmt.Printf("Status  %s\n", task.Status)
	fmt.Printf("Repo    %s\n", task.FullName())
	if task.GitHubIssueNumber != nil {
		fmt.Printf("Issue   #%d\n", *task.GitHubIssueNumber)
	}
	fmt.Printf("Title   %s\n", task.Title)
	fmt.Printf("Attempt %d\n", task.AttemptCount)
	if task.WorkerID != nil {
		fmt.Printf("Worker  %s\n", *task.WorkerID)
	}
	if task.LeaseExpiresAt != nil {
		fmt.Printf("Lease   expires %s\n", task.LeaseExpiresAt.Local().Format(time.RFC3339))
	}
	fmt.Printf("Created %s\n", task.CreatedAt.Local().Format(time.RFC3339))

	if *showPrompt {
		fmt.Printf("\n--- prompt ---\n%s\n", task.Prompt)
	}

	events, err := c.ListEvents(task.ID)
	if err != nil {
		return err
	}
	fmt.Printf("\n--- events (%d) ---\n", len(events))
	for _, e := range events {
		fmt.Printf("%s  %-8s %s\n", e.CreatedAt.Local().Format("15:04:05"), e.Type, e.Message)
	}
	return nil
}

func taskCancel(args []string) error {
	fs := flag.NewFlagSet("task cancel", flag.ExitOnError)
	serverURL := fs.String("server", defaultServer, "control plane URL")

	taskID, rest := splitPositional(args)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if taskID == "" {
		taskID = fs.Arg(0)
	}
	if taskID == "" {
		return fmt.Errorf("usage: codefactory task cancel TASK_ID")
	}

	task, err := client.New(*serverURL).CancelTask(taskID)
	if err != nil {
		return err
	}
	fmt.Printf("task %s is now %s\n", task.ID, task.Status)
	return nil
}

// ---------- shared formatting helpers ----------

func age(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
