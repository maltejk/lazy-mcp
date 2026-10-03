package client

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestStderrWatcherReportsExitWithTail(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < stderrTailLines+5; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	w := watchStderr("srv", strings.NewReader(sb.String()))

	select {
	case <-w.exited:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not report EOF")
	}

	err := w.explain("srv", errors.New("transport error"))
	msg := err.Error()
	if !strings.Contains(msg, "exited during startup") || !strings.Contains(msg, fmt.Sprintf("line %d", stderrTailLines+4)) {
		t.Fatalf("missing exit info/tail: %q", msg)
	}
	if strings.Contains(msg, "line 0\n") || strings.Contains(msg, "line 4\n") {
		t.Fatalf("tail not truncated: %q", msg)
	}
}

func TestStderrWatcherNilExplainIsNoop(t *testing.T) {
	base := errors.New("boom")
	var w *stderrWatcher
	if got := w.explain("srv", base); got != base {
		t.Fatalf("got %v", got)
	}
}
