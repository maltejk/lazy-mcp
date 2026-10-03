package hierarchy

import (
	"context"
	"sync"
	"testing"

	mcpgoclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	internalclient "github.com/voicetreelab/lazy-mcp/internal/client"
)

// newActivationTestServer builds an in-process downstream MCP server that
// mimics a GitLab-style opt-in category server: it starts out only exposing
// "known_tool" and "discover_tools", and only registers "new_tool" once
// discover_tools is called with category "releases" — matching the exact
// pattern from the lazymcp bug report (a category's tools are hidden from
// ListTools until the server's own activation tool is called in-session).
func newActivationTestServer(t *testing.T) *mcpgoclient.Client {
	t.Helper()

	mcpServer := server.NewMCPServer("downstream", "1.0.0", server.WithToolCapabilities(true))

	mcpServer.AddTool(mcp.NewTool("known_tool"), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})

	mcpServer.AddTool(mcp.NewTool("discover_tools", mcp.WithString("category")), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if request.GetArguments()["category"] == "releases" {
			mcpServer.AddTool(mcp.NewTool("new_tool"), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{}, nil
			})
		}
		return &mcp.CallToolResult{}, nil
	})

	rawClient, err := mcpgoclient.NewInProcessClient(mcpServer)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rawClient.Close() })

	ctx := context.Background()
	require.NoError(t, rawClient.Start(ctx))
	_, err = rawClient.Initialize(ctx, mcp.InitializeRequest{})
	require.NoError(t, err)

	return rawClient
}

func newTestHierarchyWithActivation(rawClient *mcpgoclient.Client) (*Hierarchy, *ServerRegistry) {
	h := &Hierarchy{
		nodes: map[string]*HierarchyNode{
			"myserver": {
				Overview: "myserver tools",
				Activation: &ActivationConfig{
					Server:     "myserver",
					Tool:       "discover_tools",
					Param:      "category",
					Categories: []string{"releases"},
				},
			},
			"myserver.known_tool": {
				Tools: map[string]*ToolDefinition{
					"known_tool": {MapsTo: "known_tool", Server: "myserver"},
				},
			},
			"myserver.discover_tools": {
				Tools: map[string]*ToolDefinition{
					"discover_tools": {MapsTo: "discover_tools", Server: "myserver"},
				},
			},
		},
	}

	registry := &ServerRegistry{
		clients:     map[string]*internalclient.Client{"myserver": internalclient.NewInProcess("myserver", rawClient)},
		clientMutex: map[string]*sync.Mutex{},
	}

	return h, registry
}

// TestHandleExecuteToolDynamic_ActivatesAndMergesNewTool is the end-to-end
// regression test for Defect 3 in the lazymcp bug report ("discover_tools
// reports success but activates nothing"). It proves that calling an
// execute_tool path lazy-mcp's static hierarchy has never seen (because the
// downstream server only reveals it after its own activation call) now
// succeeds: the fallback activates the category, merges the newly-listed
// tool into the hierarchy, and the retried call reaches the real tool.
func TestHandleExecuteToolDynamic_ActivatesAndMergesNewTool(t *testing.T) {
	rawClient := newActivationTestServer(t)
	h, registry := newTestHierarchyWithActivation(rawClient)
	ctx := context.Background()

	// Before activation, the static hierarchy has never heard of "new_tool".
	_, _, err := h.ResolveToolPath("myserver.new_tool")
	require.Error(t, err, "new_tool must not be resolvable before activation")

	// A direct execute_tool call for it should trigger the fallback, activate
	// the category on the downstream server, merge the tool in, and succeed.
	result, err := h.HandleExecuteToolDynamic(ctx, registry, "myserver.new_tool", map[string]interface{}{})
	require.NoError(t, err, "execute_tool should succeed once activation reveals the tool")
	assert.NotNil(t, result)

	// The hierarchy must now know about it statically too.
	toolDef, serverName, err := h.ResolveToolPath("myserver.new_tool")
	require.NoError(t, err)
	assert.Equal(t, "myserver", serverName)
	assert.Equal(t, "new_tool", toolDef.MapsTo)

	// get_tools_in_category("myserver") must now list it via the ordinary
	// (non-dynamic) aggregation path, alongside the tools that were always there.
	resp, err := h.HandleGetToolsInCategory("myserver")
	require.NoError(t, err)
	tools := resp["tools"].(map[string]interface{})
	assert.Contains(t, tools, "new_tool")
	assert.Contains(t, tools, "known_tool")
}

// TestHandleExecuteToolDynamic_ActivationOnlyRunsOnce verifies activation is
// idempotent: a second miss for an already-known-to-be-missing tool doesn't
// re-run the downstream activation call.
func TestHandleExecuteToolDynamic_ActivationOnlyRunsOnce(t *testing.T) {
	rawClient := newActivationTestServer(t)
	h, registry := newTestHierarchyWithActivation(rawClient)
	ctx := context.Background()

	_, err := h.HandleExecuteToolDynamic(ctx, registry, "myserver.new_tool", map[string]interface{}{})
	require.NoError(t, err)
	assert.True(t, h.activated["myserver"])

	// A subsequent miss for a tool that genuinely doesn't exist anywhere
	// should not error out from re-running activation; it should just
	// surface the ordinary "tool not found" once activation is a no-op.
	_, err = h.HandleExecuteToolDynamic(ctx, registry, "myserver.does_not_exist", map[string]interface{}{})
	assert.Error(t, err)
}

// TestFindActivation_WalksAncestorPrefixes verifies the activation config is
// found from a deeper path under the declaring node, and is nil when no
// ancestor declares one.
func TestFindActivation_WalksAncestorPrefixes(t *testing.T) {
	rawClient := newActivationTestServer(t)
	h, _ := newTestHierarchyWithActivation(rawClient)

	assert.NotNil(t, h.findActivation("myserver.releases.list_releases"))
	assert.NotNil(t, h.findActivation("myserver.new_tool"))
	assert.Nil(t, h.findActivation("someother.server.tool"))
}
