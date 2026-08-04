package api

import (
	"fmt"
	"strings"
)

// ParseFullName splits a "owner/name" repository reference.
//
// It is strict on purpose: a typo here would otherwise turn into a confusing
// GitHub API error much later in the pipeline.
func ParseFullName(full string) (owner, name string, err error) {
	trimmed := strings.TrimSpace(full)
	trimmed = strings.TrimSuffix(trimmed, "/")
	trimmed = strings.TrimSuffix(trimmed, ".git")

	// Accept a full URL for convenience: https://github.com/owner/name
	if i := strings.Index(trimmed, "github.com/"); i >= 0 {
		trimmed = trimmed[i+len("github.com/"):]
	}
	if i := strings.Index(trimmed, "github.com:"); i >= 0 {
		trimmed = trimmed[i+len("github.com:"):]
	}

	owner, name, ok := strings.Cut(trimmed, "/")
	if !ok {
		return "", "", fmt.Errorf("repository %q must be in owner/name form", full)
	}
	owner = strings.TrimSpace(owner)
	name = strings.TrimSpace(name)
	if owner == "" || name == "" {
		return "", "", fmt.Errorf("repository %q must be in owner/name form", full)
	}
	if strings.Contains(name, "/") {
		return "", "", fmt.Errorf("repository %q must be in owner/name form", full)
	}
	return owner, name, nil
}
