// Package githubcli is a thin wrapper around the `gh` command line tool.
//
// Using `gh` instead of a GitHub App is a deliberate MVP choice: it reuses the
// credentials you already have on your machine, and every call it makes is a
// command you could have typed yourself. That makes the whole GitHub layer
// easy to debug and easy to trust.
package githubcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// ErrUnavailable is returned when `gh` is missing or not authenticated.
var ErrUnavailable = errors.New("gh CLI unavailable")

// Client runs `gh` subcommands.
type Client struct {
	// Bin is the gh executable. Defaults to "gh" (resolved through PATH).
	Bin string
	// DryRun makes every mutating call log what it *would* do and return
	// success without touching GitHub. Read calls still execute.
	DryRun bool
	// Timeout bounds a single gh invocation.
	Timeout time.Duration
	// Logf receives a line for every command run. May be nil.
	Logf func(format string, args ...any)
}

// New returns a client with sensible defaults.
func New(dryRun bool) *Client {
	return &Client{Bin: "gh", DryRun: dryRun, Timeout: 60 * time.Second}
}

func (c *Client) bin() string {
	if c.Bin == "" {
		return "gh"
	}
	return c.Bin
}

func (c *Client) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// Issue is the subset of a GitHub issue the factory cares about.
type Issue struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	URL    string   `json:"url"`
	State  string   `json:"state"`
	Labels []string `json:"-"`
	Author string   `json:"-"`
}

// rawIssue matches the JSON shape `gh issue view --json` returns.
type rawIssue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	URL    string `json:"url"`
	State  string `json:"state"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
}

func (r rawIssue) toIssue() Issue {
	iss := Issue{
		Number: r.Number,
		Title:  r.Title,
		Body:   r.Body,
		URL:    r.URL,
		State:  r.State,
		Author: r.Author.Login,
	}
	for _, l := range r.Labels {
		iss.Labels = append(iss.Labels, l.Name)
	}
	return iss
}

// HasLabel reports whether the issue currently carries the given label.
func (i Issue) HasLabel(name string) bool {
	for _, l := range i.Labels {
		if strings.EqualFold(l, name) {
			return true
		}
	}
	return false
}

// run executes a gh subcommand and returns stdout.
func (c *Client) run(stdin string, args ...string) (string, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.bin(), args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	c.logf("gh %s", strings.Join(args, " "))
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return stdout.String(), fmt.Errorf("gh %s timed out after %s", args[0], timeout)
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		if errors.Is(err, exec.ErrNotFound) {
			return "", fmt.Errorf("%w: %v (install https://cli.github.com/ or use --dry-run)", ErrUnavailable, err)
		}
		return stdout.String(), fmt.Errorf("gh %s failed: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}

// Available checks that gh exists and is authenticated. Call it once at
// startup so failures surface early with an actionable message rather than
// halfway through a task.
//
// Note that --github-dry-run does NOT relax these requirements: dry-run only
// skips *writes*. Reading issues still needs an authenticated gh. To work with
// no GitHub at all, use the fake runtime with a local repository (see
// docs/DEMO.md) or pass --no-github to the worker.
func (c *Client) Available() error {
	if _, err := exec.LookPath(c.bin()); err != nil {
		return fmt.Errorf("%w: %q not found in PATH; install https://cli.github.com/ "+
			"(note: --github-dry-run still needs gh, because reading issues is a real API call)",
			ErrUnavailable, c.bin())
	}
	if _, err := c.run("", "auth", "status"); err != nil {
		return fmt.Errorf("%w: gh is installed but not authenticated; run `gh auth login` "+
			"(note: --github-dry-run still needs authentication, because it only skips writes, not reads): %v",
			ErrUnavailable, err)
	}
	return nil
}

// ---------- read operations (always executed, even in dry-run) ----------

// ListIssuesByLabel returns open issues carrying the given label.
func (c *Client) ListIssuesByLabel(repo, label string, limit int) ([]Issue, error) {
	if limit <= 0 {
		limit = 30
	}
	out, err := c.run("",
		"issue", "list",
		"--repo", repo,
		"--label", label,
		"--state", "open",
		"--limit", fmt.Sprint(limit),
		"--json", "number,title,body,url,state,labels,author",
	)
	if err != nil {
		return nil, err
	}

	var raw []rawIssue
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse gh issue list output: %w", err)
	}
	issues := make([]Issue, 0, len(raw))
	for _, r := range raw {
		issues = append(issues, r.toIssue())
	}
	return issues, nil
}

// ViewIssue fetches one issue's live state.
//
// Always call this immediately before mutating an issue: the labels stored on
// the task are a snapshot from poll time and a human may have changed them
// since.
func (c *Client) ViewIssue(repo string, number int) (Issue, error) {
	out, err := c.run("",
		"issue", "view", fmt.Sprint(number),
		"--repo", repo,
		"--json", "number,title,body,url,state,labels,author",
	)
	if err != nil {
		return Issue{}, err
	}
	var raw rawIssue
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return Issue{}, fmt.Errorf("parse gh issue view output: %w", err)
	}
	return raw.toIssue(), nil
}

// ---------- write operations (skipped in dry-run) ----------

// Comment posts a comment on an issue. The body goes in over stdin so that
// markdown, quotes and newlines survive intact.
func (c *Client) Comment(repo string, number int, body string) error {
	if c.DryRun {
		c.logf("[dry-run] would comment on %s#%d (%d bytes)", repo, number, len(body))
		return nil
	}
	_, err := c.run(body, "issue", "comment", fmt.Sprint(number), "--repo", repo, "--body-file", "-")
	return err
}

// SetLabels adds and removes labels in a single `gh issue edit` call.
func (c *Client) SetLabels(repo string, number int, add, remove []string) error {
	if len(add) == 0 && len(remove) == 0 {
		return nil
	}
	if c.DryRun {
		c.logf("[dry-run] would edit labels on %s#%d (+%v -%v)", repo, number, add, remove)
		return nil
	}

	args := []string{"issue", "edit", fmt.Sprint(number), "--repo", repo}
	for _, l := range add {
		args = append(args, "--add-label", l)
	}
	for _, l := range remove {
		args = append(args, "--remove-label", l)
	}
	_, err := c.run("", args...)
	return err
}

// EnsureLabel creates a label if it does not already exist.
func (c *Client) EnsureLabel(repo, name, description, color string) error {
	if c.DryRun {
		c.logf("[dry-run] would ensure label %q on %s", name, repo)
		return nil
	}
	if color == "" {
		color = "ededed"
	}
	// --force makes this idempotent: it updates instead of failing when the
	// label already exists.
	_, err := c.run("", "label", "create", name,
		"--repo", repo, "--description", description, "--color", color, "--force")
	return err
}

// CreateDraftPR opens a draft pull request and returns its URL.
//
// Draft is not optional in this project: a human always reviews before merge.
func (c *Client) CreateDraftPR(repo, head, base, title, body string) (string, error) {
	if c.DryRun {
		c.logf("[dry-run] would open draft PR on %s (%s -> %s): %s", repo, head, base, title)
		return "", nil
	}
	out, err := c.run(body,
		"pr", "create",
		"--repo", repo,
		"--head", head,
		"--base", base,
		"--title", title,
		"--body-file", "-",
		"--draft",
	)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(lastNonEmptyLine(out)), nil
}

// PullRequest is the subset of a PR the reviewer needs.
type PullRequest struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	URL         string `json:"url"`
	State       string `json:"state"`
	HeadRefName string `json:"headRefName"`
	BaseRefName string `json:"baseRefName"`
	IsDraft     bool   `json:"isDraft"`
	Author      string `json:"-"`
}

type rawPR struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	URL         string `json:"url"`
	State       string `json:"state"`
	HeadRefName string `json:"headRefName"`
	BaseRefName string `json:"baseRefName"`
	IsDraft     bool   `json:"isDraft"`
	Author      struct {
		Login string `json:"login"`
	} `json:"author"`
}

func (r rawPR) toPR() PullRequest {
	return PullRequest{
		Number: r.Number, Title: r.Title, Body: r.Body, URL: r.URL,
		State: r.State, HeadRefName: r.HeadRefName, BaseRefName: r.BaseRefName,
		IsDraft: r.IsDraft, Author: r.Author.Login,
	}
}

// FindPRForIssue returns the open PR the factory opened for an issue, if any.
//
// The link is the PR body: the implement stage always writes "Closes #N". That
// is more reliable than guessing from branch names, and it means a
// hand-written PR that closes the issue is reviewable too.
func (c *Client) FindPRForIssue(repo string, issueNumber int) (*PullRequest, error) {
	out, err := c.run("",
		"pr", "list",
		"--repo", repo,
		"--state", "open",
		"--limit", "50",
		"--json", "number,title,body,url,state,headRefName,baseRefName,isDraft,author",
	)
	if err != nil {
		return nil, err
	}

	var raw []rawPR
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse gh pr list output: %w", err)
	}

	// Match "#N" only when it is not part of a longer number, so #7 does not
	// match #70.
	ref := regexp.MustCompile(`#` + fmt.Sprint(issueNumber) + `\b`)
	for _, r := range raw {
		if ref.MatchString(r.Body) {
			pr := r.toPR()
			return &pr, nil
		}
	}
	return nil, nil
}

// PRDiff returns the unified diff of a pull request.
func (c *Client) PRDiff(repo string, number int) (string, error) {
	return c.run("", "pr", "diff", fmt.Sprint(number), "--repo", repo)
}

// CommentOnPR posts a comment on a pull request.
//
// This posts a normal comment rather than a formal GitHub review, on purpose:
// a formal review can carry an APPROVE that counts towards branch protection,
// and an agent should never be able to satisfy a human approval requirement.
func (c *Client) CommentOnPR(repo string, number int, body string) error {
	if c.DryRun {
		c.logf("[dry-run] would comment on PR %s#%d (%d bytes)", repo, number, len(body))
		return nil
	}
	_, err := c.run(body, "pr", "comment", fmt.Sprint(number), "--repo", repo, "--body-file", "-")
	return err
}

// DefaultBranch returns the repository's default branch name.
func (c *Client) DefaultBranch(repo string) (string, error) {
	out, err := c.run("", "repo", "view", repo, "--json", "defaultBranchRef", "-q", ".defaultBranchRef.name")
	if err != nil {
		return "", err
	}
	branch := strings.TrimSpace(out)
	if branch == "" {
		return "", fmt.Errorf("could not determine default branch for %s", repo)
	}
	return branch, nil
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}
