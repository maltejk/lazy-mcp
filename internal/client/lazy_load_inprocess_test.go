package client

import (
	"context"
	"encoding/json"
	"testing"

	mcpgoclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tbxark/optional-go"
	"github.com/voicetreelab/lazy-mcp/internal/config"
)

func startInProcess(t *testing.T, s *server.MCPServer) *mcpgoclient.Client {
	t.Helper()
	raw, err := mcpgoclient.NewInProcessClient(s)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	require.NoError(t, raw.Start(context.Background()))
	return raw
}

func listToolNames(t *testing.T, raw *mcpgoclient.Client) []string {
	t.Helper()
	res, err := raw.ListTools(context.Background(), mcp.ListToolsRequest{})
	require.NoError(t, err)
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// A lazy-loaded downstream mounts only its activate_<server> tool until that
// tool is called; calling it mounts the real catalog.
func TestLazyLoadMountsMetaToolUntilActivated(t *testing.T) {
	downstream := server.NewMCPServer("downstream", "1.0.0", server.WithToolCapabilities(true))
	noop := func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	}
	downstream.AddTool(mcp.NewTool("alpha"), noop)
	downstream.AddTool(mcp.NewTool("beta"), noop)

	c := NewInProcess("demo", startInProcess(t, downstream))
	c.options = &config.OptionsV2{LazyLoad: optional.NewField(true)}

	proxy := server.NewMCPServer("proxy", "1.0.0", server.WithToolCapabilities(true))
	ctx := context.Background()
	require.NoError(t, c.AddToMCPServer(ctx, mcp.Implementation{Name: "test"}, proxy))

	front := startInProcess(t, proxy)
	_, err := front.Initialize(ctx, mcp.InitializeRequest{})
	require.NoError(t, err)

	assert.Equal(t, []string{"activate_demo"}, listToolNames(t, front))

	call := mcp.CallToolRequest{}
	call.Params.Name = "activate_demo"
	res, err := front.CallTool(ctx, call)
	require.NoError(t, err)
	require.Len(t, res.Content, 1)
	text, ok := mcp.AsTextContent(res.Content[0])
	require.True(t, ok)
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(text.Text), &body))
	assert.Equal(t, true, body["activated"])
	assert.Equal(t, "demo", body["server"])
	assert.EqualValues(t, 2, body["toolCount"])

	assert.ElementsMatch(t, []string{"activate_demo", "alpha", "beta"}, listToolNames(t, front))

	// Activating again is a no-op that still reports success.
	res, err = front.CallTool(ctx, call)
	require.NoError(t, err)
	assert.False(t, res.IsError)
}
