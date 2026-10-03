package client

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
)

// stderrTailLines is how many trailing stderr lines are kept to explain a
// downstream server that dies during startup.
const stderrTailLines = 20

// stderrWatcher drains a downstream stdio server's stderr, keeping the last
// few lines. stderr hitting EOF means the process exited (or closed stderr),
// which lets startup fail fast with the server's own diagnostics instead of
// waiting out serverStartupTimeout.
type stderrWatcher struct {
	mu     sync.Mutex
	tail   []string
	exited chan struct{}
}

// watchStderr starts draining r. Every line is also logged under serverName,
// since a downstream's stderr is usually where it explains why it is unhappy:
// at info level when verbose is set (options.debugLogging), else at debug.
func watchStderr(serverName string, r io.Reader, verbose bool) *stderrWatcher {
	level := slog.LevelDebug
	if verbose {
		level = slog.LevelInfo
	}
	w := &stderrWatcher{exited: make(chan struct{})}
	go func() {
		defer close(w.exited)
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			slog.Log(context.Background(), level, "Downstream stderr", "client", serverName, "line", line)
			w.mu.Lock()
			w.tail = append(w.tail, line)
			if len(w.tail) > stderrTailLines {
				w.tail = w.tail[1:]
			}
			w.mu.Unlock()
		}
		// Keep draining after an oversized line so the pipe never blocks the child.
		_, _ = io.Copy(io.Discard, r)
	}()
	return w
}

func (w *stderrWatcher) lastLines() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(w.tail, "\n")
}

// exitError describes the server having exited, including its stderr tail.
func (w *stderrWatcher) exitError(serverName string) error {
	if tail := w.lastLines(); tail != "" {
		return fmt.Errorf("server %s exited during startup; stderr:\n%s", serverName, tail)
	}
	return fmt.Errorf("server %s exited during startup (no stderr output)", serverName)
}

// explain enriches a startup error with the server's exit info when it died
// (briefly waiting for stderr to hit EOF, since the transport may report the
// broken pipe before the watcher sees the exit). Returns err unchanged if the
// server is still alive.
func (w *stderrWatcher) explain(serverName string, err error) error {
	if w == nil {
		return err
	}
	select {
	case <-w.exited:
	case <-time.After(500 * time.Millisecond):
		return err
	}
	return fmt.Errorf("%w: %v", err, w.exitError(serverName))
}

// drainStderr keeps reading a stdio subprocess's stderr for as long as it runs.
//
// Start() hands the subprocess a StderrPipe that nothing consumes: mcp-go only
// exposes it through Stderr(). An OS pipe holds roughly 64KB, so a downstream
// that logs to stderr — most of them do — works until that buffer fills and then
// blocks inside write(2) forever. Nothing reports an error, because from here
// the server has simply gone silent: one stdio channel carries every request, so
// the keepalive ping stops being answered too and the client is marked unhealthy
// for every caller until the proxy restarts.
//
// The lines are logged rather than discarded, and the last few are kept so a
// downstream that dies during startup can be reported with its own diagnostics.
func drainStderr(name string, mcpClient *client.Client, verbose bool) *stderrWatcher {
	stderr, ok := client.GetStderr(mcpClient)
	if !ok || stderr == nil {
		return nil
	}
	return watchStderr(name, stderr, verbose)
}
