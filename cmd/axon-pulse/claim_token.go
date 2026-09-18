package main

import (
	"errors"
	"io"
	"strings"
)

// Bound the read to the controller's maximum token length plus an optional CRLF.
// Never include supplied data or reader errors in diagnostics: either may carry
// the secret. EOF is required so multiple lines cannot become one credential.
func readClaimToken(input io.Reader) (string, error) {
	const maxTokenBytes = 160
	data, err := io.ReadAll(io.LimitReader(input, maxTokenBytes+3))
	if err != nil {
		return "", errors.New("could not read claim token from standard input")
	}
	value := strings.TrimSpace(string(data))
	if len(data) > maxTokenBytes+2 || len(value) == 0 || len(value) > maxTokenBytes || strings.ContainsAny(value, "\r\n") {
		return "", errors.New("standard input must contain one claim token of at most 160 bytes")
	}
	return value, nil
}
