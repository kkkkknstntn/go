// Package ids generates opaque random identifiers.
package ids

import (
	"crypto/rand"
	"encoding/hex"
)

// New returns prefix and 64 random bits in hex: "r-3fa91c0d5b7e2a44".
func New(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error since Go 1.24
	return prefix + hex.EncodeToString(b)
}
