package service

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// go-winio starts one process-wide I/O completion goroutine on the
	// first named-pipe use; it is not a leak.
	goleak.VerifyTestMain(m, goleak.IgnoreAnyFunction("github.com/Microsoft/go-winio.ioCompletionProcessor"))
}
