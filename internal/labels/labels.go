// Package labels holds the GitHub issue labels that drive the factory state
// machine. The labels are the workflow: an issue's label is what decides
// whether the factory refines it, implements it, or leaves it alone.
package labels

// The label vocabulary. Everything the poller reads and every mutation the
// worker makes uses these constants.
const (
	Inbox      = "factory:inbox"       // a human wants this refined
	Refining   = "factory:refining"    // a refine task is in flight
	NeedsHuman = "factory:needs-human" // too vague to implement safely
	Ready      = "factory:ready"       // refined and safe to implement
	Active     = "factory:active"      // an implement task is in flight
	Review     = "factory:review"      // a draft PR is waiting for a human
	Blocked    = "factory:blocked"     // the agent could not finish
	Done       = "factory:done"        // finished
)

// All returns every label the factory uses, which is handy for a one-time
// `gh label create` bootstrap.
func All() []string {
	return []string{Inbox, Refining, NeedsHuman, Ready, Active, Review, Blocked, Done}
}

// Descriptions documents each label for humans browsing the repo.
var Descriptions = map[string]string{
	Inbox:      "Queued for the factory to refine into a structured ticket",
	Refining:   "A refine_ticket task is currently running",
	NeedsHuman: "Too ambiguous to implement; a human needs to clarify it",
	Ready:      "Refined and ready for an implement_ticket task",
	Active:     "An implement_ticket task is currently running",
	Review:     "A draft PR is open and waiting for human review",
	Blocked:    "The agent could not complete this ticket",
	Done:       "Completed by the factory",
}
