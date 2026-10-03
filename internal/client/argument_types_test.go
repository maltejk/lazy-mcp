package client

import (
	"context"
	"testing"

	mcpgoclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCallTool_PreservesTypedArguments is a regression guard for the
// lazymcp bug report's Defect 1 ("typed arguments arrive as strings/objects").
// Investigation traced the real cause to internal/hierarchy.HandleGetToolsInCategory
// never surfacing a tool's inputSchema up front (so callers guess types blind),
// not to any stringification in the transport. This test proves the transport
// itself — the exact mcp-go *client.Client.CallTool call that
// hierarchy.HandleExecuteTool delegates to via Client.GetClient() — preserves
// boolean, number, and array argument types end to end over an in-process
// MCP connection, so any future regression here is caught immediately.
func TestCallTool_PreservesTypedArguments(t *testing.T) {
	var received map[string]interface{}

	mcpServer := server.NewMCPServer("probe-server", "1.0.0", server.WithToolCapabilities(true))
	mcpServer.AddTool(mcp.NewTool("probe"), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		received = request.GetArguments()
		return &mcp.CallToolResult{}, nil
	})

	rawClient, err := mcpgoclient.NewInProcessClient(mcpServer)
	require.NoError(t, err)
	defer rawClient.Close()

	ctx := context.Background()
	require.NoError(t, rawClient.Start(ctx))
	_, err = rawClient.Initialize(ctx, mcp.InitializeRequest{})
	require.NoError(t, err)

	c := &Client{name: "probe-server", client: rawClient}

	callRequest := mcp.CallToolRequest{}
	callRequest.Params.Name = "probe"
	callRequest.Params.Arguments = map[string]interface{}{
		"nameOnly": true,
		"maxCount": float64(25),
		"paths":    []interface{}{"a.txt", "b.txt"},
		"message":  "plain string",
	}

	_, err = c.GetClient().CallTool(ctx, callRequest)
	require.NoError(t, err)

	require.NotNil(t, received, "tool handler should have been invoked")
	assert.Equal(t, true, received["nameOnly"], "boolean argument must not be stringified")
	assert.Equal(t, float64(25), received["maxCount"], "number argument must not be stringified")
	assert.Equal(t, []interface{}{"a.txt", "b.txt"}, received["paths"], "array argument must not become a plain object/string")
	assert.Equal(t, "plain string", received["message"])
}
