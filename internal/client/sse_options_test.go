package client

import (
	"testing"
	"time"

	"github.com/voicetreelab/lazy-mcp/internal/config"
)

// The timeout used to be parsed but never applied to sse clients.
func TestSSEConfigCarriesTimeout(t *testing.T) {
	t.Parallel()

	conf := &config.MCPClientConfigV2{
		TransportType: config.MCPClientTypeSSE,
		URL:           "https://example.com/sse",
		Timeout:       config.Duration(7 * time.Second),
	}
	parsed, err := config.ParseMCPClientConfigV2(conf)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	sse, ok := parsed.(*config.SSEMCPClientConfig)
	if !ok {
		t.Fatalf("type = %T, want *SSEMCPClientConfig", parsed)
	}
	if time.Duration(sse.Timeout) != 7*time.Second {
		t.Errorf("sse timeout = %v, want 7s", time.Duration(sse.Timeout))
	}
	if got := len(sseClientOptions(sse)); got != 2 {
		t.Errorf("sse client options = %d, want 2 (headers and timeout)", got)
	}
}
