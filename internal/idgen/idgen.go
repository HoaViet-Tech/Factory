// Package idgen produces the short, URL-safe identifiers used for tasks,
// workers and lease tokens.
package idgen

import (
	"crypto/rand"
	"encoding/hex"
)

// New returns a random lowercase hex string of n bytes (2n characters).
func New(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail on supported platforms; if it ever does
		// there is nothing sensible to recover to, so panic loudly.
		panic("idgen: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// TaskID returns a 12-character task identifier, short enough to type by hand.
func TaskID() string { return New(6) }

// WorkerID returns a 16-character worker identifier.
func WorkerID() string { return New(8) }

// LeaseToken returns a 32-character lease token. A worker must present this
// token to append events to or complete the task it claimed.
func LeaseToken() string { return New(16) }

// Short returns the first n characters of s, used for branch names.
func Short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
