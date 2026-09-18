package main

import (
	"errors"
	"io"
	"strings"
	"testing"
)

type (
	failedTokenReader struct{}
)

func (failedTokenReader) Read([]byte) (int, error) {
	return 0, errors.New("spt_secret_in_reader_error")
}

func TestReadClaimToken(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		input io.Reader
		want  string
	}{
		{"plain", strings.NewReader("spt_example"), "spt_example"},
		{"newline", strings.NewReader("spt_example\n"), "spt_example"},
		{"maximum with CRLF", strings.NewReader(strings.Repeat("s", 160) + "\r\n"), strings.Repeat("s", 160)},
		{"empty", strings.NewReader(" \n"), ""},
		{"multiple lines", strings.NewReader("spt_one\nspt_two"), ""},
		{"too long", strings.NewReader(strings.Repeat("s", 161)), ""},
		{"read error", failedTokenReader{}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := readClaimToken(test.input)
			if got != test.want || (err != nil) != (test.want == "") {
				t.Fatalf("readClaimToken returned unexpected value or error: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "spt_") {
				t.Fatal("reader error exposed a token")
			}
		})
	}
	large := strings.NewReader(strings.Repeat("s", 1024*1024))
	if _, err := readClaimToken(large); err == nil || large.Len() != 1024*1024-163 {
		t.Fatal("stdin read was not bounded")
	}
}
