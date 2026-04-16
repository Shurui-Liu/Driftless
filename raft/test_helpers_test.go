package raft

import (
	"io"
	"log/slog"
)

// newTestLogger returns a silent logger suitable for unit tests.
// Swallows all output so test runs stay clean.
func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
