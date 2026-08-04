package api

import "testing"

func TestParseFullName(t *testing.T) {
	cases := []struct {
		in          string
		owner, name string
	}{
		{"owainlewis/factory", "owainlewis", "factory"},
		{"  owner/name  ", "owner", "name"},
		{"owner/name.git", "owner", "name"},
		{"owner/name/", "owner", "name"},
		{"https://github.com/owner/name", "owner", "name"},
		{"https://github.com/owner/name.git", "owner", "name"},
		{"git@github.com:owner/name.git", "owner", "name"},
	}
	for _, c := range cases {
		owner, name, err := ParseFullName(c.in)
		if err != nil {
			t.Errorf("ParseFullName(%q) returned error: %v", c.in, err)
			continue
		}
		if owner != c.owner || name != c.name {
			t.Errorf("ParseFullName(%q) = %q/%q, want %q/%q", c.in, owner, name, c.owner, c.name)
		}
	}
}

func TestParseFullNameRejectsBadInput(t *testing.T) {
	bad := []string{"", "   ", "justaname", "/name", "owner/", "a/b/c"}
	for _, in := range bad {
		if _, _, err := ParseFullName(in); err == nil {
			t.Errorf("ParseFullName(%q) should have failed", in)
		}
	}
}

func TestIsTerminal(t *testing.T) {
	terminal := []string{StatusSucceeded, StatusFailed, StatusCancelled, StatusLost}
	for _, s := range terminal {
		if !IsTerminal(s) {
			t.Errorf("%s should be terminal", s)
		}
	}
	for _, s := range []string{StatusQueued, StatusRunning} {
		if IsTerminal(s) {
			t.Errorf("%s should not be terminal", s)
		}
	}
}
