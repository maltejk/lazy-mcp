package e2e

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// These tests drive the command line the way an operator does: flag dispatch,
// the -check-config and -auth-status reports, and their exit statuses.

func TestProxyCLI(t *testing.T) {
	skipShort(t)
	t.Parallel()

	validConfig, _ := stdioConfig(t, "streamable-http")

	t.Run("version", func(t *testing.T) {
		t.Parallel()

		stdout, _, err := runCLI(t, "-version")
		if err != nil {
			t.Fatalf("-version: %v", err)
		}
		if strings.TrimSpace(stdout) == "" {
			t.Error("-version printed nothing")
		}
	})

	t.Run("check-config accepts a valid config", func(t *testing.T) {
		t.Parallel()

		stdout, stderr, err := runCLI(t, "-check-config", "-config", validConfig)
		if err != nil {
			t.Fatalf("-check-config: %v\n%s", err, stderr)
		}
		// "fixture" and the disabled "off" server both count as configured.
		if !strings.Contains(stdout, "Config OK: 2 MCP server(s)") {
			t.Errorf("-check-config stdout = %q, want the OK summary", stdout)
		}
	})

	// A server name with a path separator would escape the proxy's base path,
	// so it has to be rejected before the daemon ever starts.
	t.Run("check-config rejects a route-escaping server name", func(t *testing.T) {
		t.Parallel()

		bad := writeConfig(t, `{
  "mcpProxy": {
    "baseURL": "http://127.0.0.1:9999",
    "addr": "127.0.0.1:9999",
    "name": "p",
    "version": "1",
    "type": "streamable-http"
  },
  "mcpServers": {"../escape": {"command": "true"}}
}`)
		_, stderr, err := runCLI(t, "-check-config", "-config", bad)
		if err == nil {
			t.Fatal("-check-config accepted a server name containing a path separator")
		}
		if !strings.Contains(stderr, "../escape") {
			t.Errorf("stderr = %q, want the offending server name", stderr)
		}
	})

	// A bare number means nanoseconds, so "timeout": 30 used to silently break
	// every request to that server.
	t.Run("check-config rejects a nanosecond timeout", func(t *testing.T) {
		t.Parallel()

		bad := writeConfig(t, `{
  "mcpProxy": {
    "baseURL": "http://127.0.0.1:9999",
    "addr": "127.0.0.1:9999",
    "name": "p",
    "version": "1",
    "type": "streamable-http"
  },
  "mcpServers": {
    "remote": {"transportType": "streamable-http", "url": "https://example.com/mcp", "timeout": 30}
  }
}`)
		_, stderr, err := runCLI(t, "-check-config", "-config", bad)
		if err == nil {
			t.Fatal("-check-config accepted a 30ns timeout")
		}
		if !strings.Contains(stderr, "nanoseconds") || !strings.Contains(stderr, "30s") {
			t.Errorf("stderr = %q, want a hint about the unit and a duration string", stderr)
		}
	})

	t.Run("check-config rejects a missing file", func(t *testing.T) {
		t.Parallel()

		if _, _, err := runCLI(t, "-check-config", "-config", "/nonexistent/config.json"); err == nil {
			t.Fatal("-check-config accepted a missing config file")
		}
	})

	// -auth-status is local-only, so it must succeed without any network.
	t.Run("auth-status reports every server", func(t *testing.T) {
		t.Parallel()

		stdout, stderr, err := runCLI(t, "-auth-status", "-config", validConfig)
		if err != nil {
			t.Fatalf("-auth-status: %v\n%s", err, stderr)
		}
		for _, want := range []string{"NAME", "fixture", "off", "stdio", "no auth"} {
			if !strings.Contains(stdout, want) {
				t.Errorf("-auth-status output missing %q:\n%s", want, stdout)
			}
		}
	})
}

// A downstream server that cannot start must not take the whole proxy down:
// creating a stdio client spawns the subprocess, so a missing command fails
// before the connection is ever attempted.
func TestBrokenServerDoesNotStopTheProxy(t *testing.T) {
	skipShort(t)
	t.Parallel()

	addr := freeAddr(t)
	fixture := filepath.ToSlash(buildFixture(t))
	configPath := writeConfig(t, fmt.Sprintf(`{
  "mcpProxy": {
    "baseURL": "http://%[1]s", "addr": "%[1]s", "name": "p", "version": "1",
    "type": "streamable-http", "startupGracePeriod": "`+testGracePeriod+`"
  },
  "mcpServers": {
    "broken": {"command": "/nonexistent/definitely-not-a-real-command"},
    "fixture": {"command": "%[2]s"}
  }
}`, addr, fixture))

	// startProxy waits for readiness, so reaching this line is the assertion.
	proxy := startProxy(t, configPath, addr)

	if got := proxy.get(t, "/broken/mcp"); got != http.StatusNotFound {
		t.Errorf("broken server route = %d, want 404 (it must never be mounted)", got)
	}
	mcpClient := proxy.connect(t, "streamable-http", "fixture")
	if got := callToolText(t, mcpClient, "echo", map[string]any{"message": "still up"}); got != "still up" {
		t.Errorf("echo through the working server = %q, want %q", got, "still up")
	}
	proxy.stop(t)
}

// ...unless that server set panicIfInvalid, which stays fatal for the process.
func TestPanicIfInvalidStopsTheProxy(t *testing.T) {
	skipShort(t)
	t.Parallel()

	addr := freeAddr(t)
	configPath := writeConfig(t, fmt.Sprintf(`{
  "mcpProxy": {
    "baseURL": "http://%[1]s", "addr": "%[1]s", "name": "p", "version": "1",
    "type": "streamable-http"
  },
  "mcpServers": {
    "broken": {
      "command": "/nonexistent/definitely-not-a-real-command",
      "options": {"panicIfInvalid": true}
    }
  }
}`, addr))

	_, stderr, err := runCLI(t, "-config", configPath)
	if err == nil {
		t.Fatal("proxy exited 0 with a fatal client failure, want a non-zero status")
	}
	if !strings.Contains(stderr, "failed to initialize clients") {
		t.Errorf("stderr = %q, want the client failure reason", stderr)
	}
}
