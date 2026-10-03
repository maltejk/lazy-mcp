package hierarchy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mixedSchema is a representative inputSchema mixing string, boolean, number,
// and array parameter types, matching the shapes reported in the lazymcp bug
// report (e.g. git_diff's nameOnly boolean, git_reflog's maxCount number,
// git_add's paths array).
func mixedSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"nameOnly": map[string]interface{}{"type": "boolean"},
			"maxCount": map[string]interface{}{"type": "number"},
			"paths":    map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
		},
	}
}

// TestHandleGetToolsInCategory_DirectTools_IncludesInputSchema verifies that
// a category with tools declared directly on the node (the "toolsInfo" branch)
// surfaces each tool's full inputSchema, so a calling agent can see parameter
// types (bool/number/array) before ever calling execute_tool, instead of only
// discovering them reactively from a failed call's error message.
func TestHandleGetToolsInCategory_DirectTools_IncludesInputSchema(t *testing.T) {
	h := &Hierarchy{
		nodes: map[string]*HierarchyNode{
			"myserver": {
				Tools: map[string]*ToolDefinition{
					"git_diff": {
						Description: "Show diff",
						MapsTo:      "git_diff",
						Server:      "myserver",
						InputSchema: mixedSchema(),
					},
				},
			},
		},
	}

	resp, err := h.HandleGetToolsInCategory("myserver")
	require.NoError(t, err)

	tools, ok := resp["tools"].(map[string]interface{})
	require.True(t, ok, "expected tools map in response")

	toolInfo, ok := tools["git_diff"].(map[string]interface{})
	require.True(t, ok, "expected git_diff entry in tools map")

	assert.Equal(t, mixedSchema(), toolInfo["inputSchema"],
		"inputSchema must be surfaced up front so callers know parameter types before calling execute_tool")
}

// TestHandleGetToolsInCategory_AggregatedLeafChildren_IncludesInputSchema
// verifies the same for the "flat" hierarchy layout (a branch node whose
// children are all single-tool leaves, e.g. structure/gitlab_po/*.json),
// which aggregates child tools into the branch's own response.
func TestHandleGetToolsInCategory_AggregatedLeafChildren_IncludesInputSchema(t *testing.T) {
	h := &Hierarchy{
		nodes: map[string]*HierarchyNode{
			"myserver": {
				Overview: "myserver tools",
			},
			"myserver.git_add": {
				Tools: map[string]*ToolDefinition{
					"git_add": {
						Description: "Stage files",
						MapsTo:      "git_add",
						Server:      "myserver",
						InputSchema: mixedSchema(),
					},
				},
			},
		},
	}

	resp, err := h.HandleGetToolsInCategory("myserver")
	require.NoError(t, err)

	tools, ok := resp["tools"].(map[string]interface{})
	require.True(t, ok, "expected tools map in response")

	toolInfo, ok := tools["git_add"].(map[string]interface{})
	require.True(t, ok, "expected git_add entry in aggregated tools map")

	assert.Equal(t, mixedSchema(), toolInfo["inputSchema"],
		"aggregated leaf-child tools must also surface inputSchema up front")
}

// TestHandleGetToolsInCategory_OmitsInputSchemaWhenAbsent verifies the
// response stays clean (no null/empty inputSchema key) for tools that never
// declared one, e.g. meta-tools without typed parameters.
func TestHandleGetToolsInCategory_OmitsInputSchemaWhenAbsent(t *testing.T) {
	h := &Hierarchy{
		nodes: map[string]*HierarchyNode{
			"myserver": {
				Tools: map[string]*ToolDefinition{
					"whoami": {
						Description: "Get current user",
						MapsTo:      "whoami",
						Server:      "myserver",
					},
				},
			},
		},
	}

	resp, err := h.HandleGetToolsInCategory("myserver")
	require.NoError(t, err)

	tools := resp["tools"].(map[string]interface{})
	toolInfo := tools["whoami"].(map[string]interface{})

	_, present := toolInfo["inputSchema"]
	assert.False(t, present, "inputSchema key should be omitted, not present as nil/empty, when the tool declares none")
}
