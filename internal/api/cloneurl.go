package api

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// remoteHelperPrefix matches git's `transport::address` remote-helper syntax,
// as in `ext::sh -c payload`.
//
// This is the dangerous one: `ext::` tells git to *run a command* and speak the
// git protocol over its stdio, so a clone URL becomes arbitrary code execution.
// It is matched before URL parsing because "ext::sh -c ..." is not a URL.
var remoteHelperPrefix = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*::`)

// allowedSchemes are the URL schemes that address a real repository.
//
// file:// is permitted because plain local paths are permitted too — the
// credential-free demo clones from a directory — so rejecting it would buy
// nothing while breaking a documented workflow.
var allowedSchemes = map[string]bool{
	"https": true,
	"http":  true,
	"ssh":   true,
	"git":   true,
	"file":  true,
}

// ValidateCloneURL rejects clone URLs that git would treat as something other
// than a repository address.
//
// The worker hands this string straight to `git clone`, and git accepts inputs
// that are really commands. Two shapes matter:
//
//   - `ext::sh -c '...'` runs a command (the remote-helper syntax above).
//   - `--upload-pack=...` is read as a *flag*, not a URL, because it starts
//     with a dash — argument injection into the clone itself.
//
// Everything else that looks like a repository is allowed: https, ssh, scp-like
// (git@host:owner/repo), and local paths including Windows drive letters.
func ValidateCloneURL(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fmt.Errorf("clone URL is empty")
	}

	// Anything git could read as an option rather than an address.
	if strings.HasPrefix(trimmed, "-") {
		return fmt.Errorf("clone URL %q starts with '-', which git would read as a command-line flag", raw)
	}

	// Control characters and newlines have no place in a URL and can confuse
	// anything that later logs or re-parses it.
	if strings.ContainsAny(trimmed, "\x00\r\n") {
		return fmt.Errorf("clone URL contains control characters")
	}

	// `scheme::rest` is git's remote-helper syntax. Windows paths like
	// `C:\repos\demo` contain a single colon and are unaffected.
	if remoteHelperPrefix.MatchString(trimmed) {
		helper := trimmed[:strings.Index(trimmed, "::")]
		return fmt.Errorf(
			"clone URL uses git's %q remote-helper syntax, which executes a command rather than "+
				"addressing a repository; use an https://, ssh:// or file path URL instead", helper)
	}

	// A scheme-bearing URL must use a transport we recognise. Anything exotic
	// is refused rather than guessed at.
	if i := strings.Index(trimmed, "://"); i > 0 {
		scheme := strings.ToLower(trimmed[:i])
		if !allowedSchemes[scheme] {
			return fmt.Errorf("clone URL scheme %q is not allowed (allowed: file, git, http, https, ssh)", scheme)
		}
		if _, err := url.Parse(trimmed); err != nil {
			return fmt.Errorf("clone URL is not a valid URL: %w", err)
		}
		return nil
	}

	// No scheme: scp-like (git@host:path) or a local path. Both are fine.
	return nil
}
