//go:build windows

package state

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

func defaultSocketPath(dir string) string {
	digest := sha256.Sum256([]byte(strings.ToLower(dir)))
	return `\\.\pipe\axon-pulse-` + hex.EncodeToString(digest[:6])
}
