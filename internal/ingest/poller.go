// Package ingest turns GitHub issues into factory tasks.
//
// The poller is deliberately the *only* place that creates GitHub-triggered
// tasks, and it always goes through the store's dedupe key. Polling is
// at-least-once; task creation is exactly-once.
package ingest

import (
	"fmt"
	"log"

	"github.com/HoaViet-Tech/factory/internal/api"
	"github.com/HoaViet-Tech/factory/internal/githubcli"
	"github.com/HoaViet-Tech/factory/internal/labels"
	"github.com/HoaViet-Tech/factory/internal/prompt"
	"github.com/HoaViet-Tech/factory/internal/store"
)

// IssueLister is the slice of githubcli the poller needs. Keeping it an
// interface makes the poller testable without a real GitHub account.
type IssueLister interface {
	ListIssuesByLabel(repo, label string, limit int) ([]githubcli.Issue, error)
}

// Poller reads labelled issues and enqueues tasks.
type Poller struct {
	Store  *store.Store
	GitHub IssueLister
	// Limit caps how many issues are read per label per repository.
	Limit  int
	DryRun bool
	Logger *log.Logger
}

// workflow maps a label to the kind of task it produces.
type workflow struct {
	label      string
	kind       string
	buildTitle func(githubcli.Issue) string
	buildBody  func(repo string, iss githubcli.Issue) string
}

func workflows() []workflow {
	return []workflow{
		{
			label: labels.Inbox,
			kind:  api.KindRefineTicket,
			buildTitle: func(iss githubcli.Issue) string {
				return fmt.Sprintf("Refine #%d: %s", iss.Number, iss.Title)
			},
			buildBody: func(repo string, iss githubcli.Issue) string {
				return prompt.ForRefine(toContext(repo, iss))
			},
		},
		{
			label: labels.Ready,
			kind:  api.KindImplementTicket,
			buildTitle: func(iss githubcli.Issue) string {
				return fmt.Sprintf("Implement #%d: %s", iss.Number, iss.Title)
			},
			buildBody: func(repo string, iss githubcli.Issue) string {
				return prompt.ForImplement(toContext(repo, iss))
			},
		},
		{
			// The implement stage labels an issue factory:review once its draft
			// PR is open, so this workflow picks the PR up for review.
			//
			// The prompt here is a placeholder: the worker rebuilds it once it
			// has resolved the actual PR and fetched the live diff, neither of
			// which exists at poll time.
			label: labels.Review,
			kind:  api.KindReviewPR,
			buildTitle: func(iss githubcli.Issue) string {
				return fmt.Sprintf("Review PR for #%d: %s", iss.Number, iss.Title)
			},
			buildBody: func(repo string, iss githubcli.Issue) string {
				return prompt.ForReview(toContext(repo, iss), prompt.PRContext{})
			},
		},
	}
}

func toContext(repo string, iss githubcli.Issue) prompt.IssueContext {
	return prompt.IssueContext{
		Repo:   repo,
		Number: iss.Number,
		Title:  iss.Title,
		Body:   iss.Body,
		Author: iss.Author,
		URL:    iss.URL,
	}
}

func (p *Poller) logf(format string, args ...any) {
	if p.Logger != nil {
		p.Logger.Printf(format, args...)
	}
}

// PollOnce runs one full polling pass over every enabled repository.
func (p *Poller) PollOnce() (api.PollResponse, error) {
	resp := api.PollResponse{DryRun: p.DryRun, CreatedTaskIDs: []string{}}

	repos, err := p.Store.ListRepositories(true)
	if err != nil {
		return resp, err
	}
	resp.Repositories = len(repos)
	if len(repos) == 0 {
		return resp, nil
	}
	if p.GitHub == nil {
		return resp, fmt.Errorf("github client not configured")
	}

	for _, repo := range repos {
		for _, wf := range workflows() {
			issues, err := p.GitHub.ListIssuesByLabel(repo.FullName(), wf.label, p.Limit)
			if err != nil {
				msg := fmt.Sprintf("%s [%s]: %v", repo.FullName(), wf.label, err)
				resp.Errors = append(resp.Errors, msg)
				p.logf("poll error: %s", msg)
				continue
			}

			for _, iss := range issues {
				resp.IssuesSeen++

				key := store.DedupeKey(repo.Owner, repo.Name, iss.Number, wf.kind)
				issueNumber := iss.Number
				task, created, err := p.Store.CreateTaskForIssue(key, api.CreateTaskRequest{
					Kind:              wf.kind,
					RepoOwner:         repo.Owner,
					RepoName:          repo.Name,
					GitHubIssueNumber: &issueNumber,
					Title:             wf.buildTitle(iss),
					Prompt:            wf.buildBody(repo.FullName(), iss),
				})
				if err != nil {
					msg := fmt.Sprintf("%s#%d: %v", repo.FullName(), iss.Number, err)
					resp.Errors = append(resp.Errors, msg)
					p.logf("poll error: %s", msg)
					continue
				}
				if !created {
					resp.Skipped++
					continue
				}

				resp.TasksCreated++
				resp.CreatedTaskIDs = append(resp.CreatedTaskIDs, task.ID)
				p.logf("created %s task %s for %s#%d", wf.kind, task.ID, repo.FullName(), iss.Number)
			}
		}
	}
	return resp, nil
}
