package hierarchy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/voicetreelab/lazy-mcp/internal/client"
	"github.com/voicetreelab/lazy-mcp/internal/config"
)

// HierarchyNode represents a node in the tool hierarchy
// Can be a branch node (has children) or leaf node (has tools)
type HierarchyNode struct {
	Overview   string                     `json:"overview,omitempty"`
	Tools      map[string]*ToolDefinition `json:"tools,omitempty"`
	MCPServer  *MCPServerRef              `json:"mcp_server,omitempty"`
	Activation *ActivationConfig          `json:"activation,omitempty"`
}

// ToolDefinition represents a tool in the hierarchy
type ToolDefinition struct {
	Description string                 `json:"description,omitempty"`
	MapsTo      string                 `json:"maps_to,omitempty"`
	Server      string                 `json:"server,omitempty"`
	InputSchema map[string]interface{} `json:"inputSchema,omitempty"`
}

// ActivationConfig declares how to reveal a downstream server's additional,
// opt-in tool categories at runtime. Some downstream MCP servers hide most of
// their tools behind their own "discover"/"activate" tool (e.g. a GitLab MCP
// server's own `discover_tools`) and only reveal the rest after it's called in
// the same session. lazy-mcp's hierarchy is a static, load-once-at-startup
// snapshot (see LoadHierarchy), so without this, tools revealed that way are
// permanently unreachable through get_tools_in_category/execute_tool even
// though the downstream server's own live session now has them active.
type ActivationConfig struct {
	// Server is the downstream MCP server name to activate categories on.
	Server string `json:"server"`
	// Tool is the downstream server's own activation tool name, called directly
	// (not through maps_to), e.g. "discover_tools".
	Tool string `json:"tool"`
	// Param is the argument name that tool expects the category value under,
	// e.g. "category".
	Param string `json:"param"`
	// Categories lists every opt-in category name to activate. All of them are
	// attempted, idempotently, the first time a lookup misses under this node.
	Categories []string `json:"categories"`
}

// HierarchyNodeData is used for unmarshaling JSON with flexible tool types
type HierarchyNodeData struct {
	Overview   string                 `json:"overview,omitempty"`
	Tools      map[string]interface{} `json:"tools,omitempty"`
	MCPServer  *MCPServerRef          `json:"mcp_server,omitempty"`
	Activation *ActivationConfig      `json:"activation,omitempty"`
}

// MCPServerRef contains MCP server configuration
type MCPServerRef struct {
	Name         string            `json:"name"`
	Type         string            `json:"type"` // "stdio", "sse", "streamable-http"
	Command      string            `json:"command,omitempty"`
	Args         []string          `json:"args,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	URL          string            `json:"url,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	ToolMappings map[string]string `json:"tool_mappings,omitempty"` // Maps hierarchy tool names to actual MCP tool names
}

// ToClientConfig converts MCPServerRef to MCPClientConfigV2
func (m *MCPServerRef) ToClientConfig() *config.MCPClientConfigV2 {
	cfg := &config.MCPClientConfigV2{
		Options: &config.OptionsV2{},
	}

	switch m.Type {
	case "stdio":
		cfg.TransportType = config.MCPClientTypeStdio
		cfg.Command = m.Command
		cfg.Args = m.Args
		cfg.Env = m.Env
	case "sse":
		cfg.TransportType = config.MCPClientTypeSSE
		cfg.URL = m.URL
		cfg.Headers = m.Headers
	case "streamable-http":
		cfg.TransportType = config.MCPClientTypeStreamable
		cfg.URL = m.URL
		cfg.Headers = m.Headers
	}

	return cfg
}

// Hierarchy manages the hierarchical tool structure
type Hierarchy struct {
	rootPath string
	nodes    map[string]*HierarchyNode
	mu       sync.RWMutex
	// activated tracks which servers' ActivationConfig has already been run,
	// so a repeatedly-missing lookup doesn't re-trigger the activation sequence.
	activated map[string]bool
}

// LoadHierarchy loads the hierarchy from a directory structure
func LoadHierarchy(hierarchyPath string) (*Hierarchy, error) {
	h := &Hierarchy{
		rootPath: hierarchyPath,
		nodes:    make(map[string]*HierarchyNode),
	}

	// Load root.json
	rootFile := filepath.Join(hierarchyPath, "root.json")
	rootNode, err := loadNode(rootFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load root node: %w", err)
	}
	h.nodes[""] = rootNode
	h.nodes["/"] = rootNode

	// Walk the directory structure and load all nodes
	err = filepath.Walk(hierarchyPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(info.Name(), ".json") {
			return nil
		}
		if info.Name() == "root.json" {
			return nil // Already loaded
		}

		// Calculate the hierarchy path from the file path
		relPath, err := filepath.Rel(hierarchyPath, filepath.Dir(path))
		if err != nil {
			return err
		}

		// Get filename without extension
		filename := strings.TrimSuffix(filepath.Base(path), ".json")

		// Get the directory name
		dirname := filepath.Base(filepath.Dir(path))

		// Determine hierarchy key based on structure
		var hierarchyKey string
		if filename == dirname {
			// Nested structure: directory/directory.json → use directory path only
			// e.g., everything/everything.json → "everything"
			hierarchyKey = strings.ReplaceAll(relPath, string(filepath.Separator), ".")
			if hierarchyKey == "." {
				hierarchyKey = ""
			}
		} else {
			// Flat structure: directory/tool.json → use directory.tool
			// e.g., everything/add.json → "everything.add"
			dirKey := strings.ReplaceAll(relPath, string(filepath.Separator), ".")
			if dirKey == "." || dirKey == "" {
				hierarchyKey = filename
			} else {
				hierarchyKey = dirKey + "." + filename
			}
		}

		node, err := loadNode(path)
		if err != nil {
			log.Printf("Warning: failed to load node at %s: %v", path, err)
			return nil // Continue loading other nodes
		}

		h.nodes[hierarchyKey] = node
		log.Printf("Loaded hierarchy node: %s from %s", hierarchyKey, path)
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to walk hierarchy: %w", err)
	}

	log.Printf("Loaded %d hierarchy nodes", len(h.nodes))
	return h, nil
}

// loadNode loads a single node from a JSON file
func loadNode(path string) (*HierarchyNode, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var nodeData HierarchyNodeData
	if err := json.Unmarshal(data, &nodeData); err != nil {
		return nil, err
	}

	// Convert to HierarchyNode with typed tools
	node := &HierarchyNode{
		Overview:   nodeData.Overview,
		Tools:      make(map[string]*ToolDefinition),
		MCPServer:  nodeData.MCPServer,
		Activation: nodeData.Activation,
	}

	// Parse tools - can be either map[string]interface{} or direct ToolDefinition
	for toolName, toolData := range nodeData.Tools {
		if toolMap, ok := toolData.(map[string]interface{}); ok {
			tool := &ToolDefinition{}
			if desc, ok := toolMap["description"].(string); ok {
				tool.Description = desc
			}
			if mapsTo, ok := toolMap["maps_to"].(string); ok {
				tool.MapsTo = mapsTo
			} else {
				// Default maps_to is the tool name itself
				tool.MapsTo = toolName
			}
			if server, ok := toolMap["server"].(string); ok {
				tool.Server = server
			}
			if schema, ok := toolMap["inputSchema"].(map[string]interface{}); ok {
				tool.InputSchema = schema
			}
			node.Tools[toolName] = tool
		}
	}

	return node, nil
}

// GetRootNode returns the root node of the hierarchy
func (h *Hierarchy) GetRootNode() *HierarchyNode {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.nodes[""]
}

// normalizeCategoryPath applies the same normalization HandleGetToolsInCategory
// uses to its path argument, so callers that need to look up a node by the same
// key (e.g. findActivation) stay in sync with it.
func normalizeCategoryPath(path string) string {
	if path == "/" {
		path = ""
	}
	return strings.Trim(path, ".")
}

// HandleGetToolsInCategory handles the get_tools_in_category meta-tool
// Returns a map with path, overview, children info, and tools
func (h *Hierarchy) HandleGetToolsInCategory(path string) (map[string]interface{}, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	path = normalizeCategoryPath(path)

	// Find the node
	node, exists := h.nodes[path]
	if !exists {
		return nil, fmt.Errorf("category not found: %s", path)
	}

	// Build response
	response := map[string]interface{}{
		"path": path,
	}

	if node.Overview != "" {
		response["overview"] = node.Overview
	}

	// Find child nodes
	children := make(map[string]interface{})
	allChildrenAreLeaves := true
	aggregatedTools := make(map[string]interface{})

	for nodePath := range h.nodes {
		if nodePath == path || nodePath == "" {
			continue
		}

		// Check if this node is a direct child of the current path
		var isDirectChild bool
		var childName string

		if path == "" {
			// Root level - direct children have no dots
			if !strings.Contains(nodePath, ".") {
				isDirectChild = true
				childName = nodePath
			}
		} else {
			// Non-root - check if path is a prefix and child is one level deeper
			if strings.HasPrefix(nodePath, path+".") {
				remainder := strings.TrimPrefix(nodePath, path+".")
				if !strings.Contains(remainder, ".") {
					isDirectChild = true
					childName = remainder
				}
			}
		}

		if isDirectChild {
			childNode := h.nodes[nodePath]
			if len(childNode.Tools) > 0 {
				// Leaf node
				children[childName] = map[string]interface{}{
					"is_leaf":    true,
					"tool_count": len(childNode.Tools),
				}

				// Aggregate tools from leaf children
				for toolName, toolDef := range childNode.Tools {
					// In flat structure, nodePath already includes the tool name
					// e.g., "everything.echo" not "everything.echo.echo"
					toolPath := nodePath

					toolInfo := map[string]interface{}{
						"description": toolDef.Description,
						"tool_path":   toolPath,
					}
					if toolDef.InputSchema != nil {
						toolInfo["inputSchema"] = toolDef.InputSchema
					}
					aggregatedTools[toolName] = toolInfo
				}
			} else {
				// Branch node
				allChildrenAreLeaves = false
				childInfo := map[string]interface{}{}
				if childNode.Overview != "" {
					childInfo["overview"] = childNode.Overview
				}
				children[childName] = childInfo
			}
		}
	}

	if len(children) > 0 {
		response["children"] = children
	}

	// If this node has direct tools or all children are leaves, include tools
	if len(node.Tools) > 0 {
		// Node has direct tools
		toolsInfo := make(map[string]interface{})
		for toolName, toolDef := range node.Tools {
			var toolPath string
			if path == "" {
				toolPath = toolName
			} else {
				toolPath = path + "." + toolName
			}

			toolInfo := map[string]interface{}{
				"description": toolDef.Description,
				"tool_path":   toolPath,
			}
			if toolDef.InputSchema != nil {
				toolInfo["inputSchema"] = toolDef.InputSchema
			}
			toolsInfo[toolName] = toolInfo
		}
		response["tools"] = toolsInfo
	} else if allChildrenAreLeaves && len(aggregatedTools) > 0 {
		// All children are leaves - include their tools
		response["tools"] = aggregatedTools
	} else {
		response["tools"] = make(map[string]interface{})
	}

	return response, nil
}

// ResolveToolPath resolves a tool path to its definition and server name
// Returns the tool definition, server name (empty for meta-tools or if not configured), and any error
func (h *Hierarchy) ResolveToolPath(toolPath string) (*ToolDefinition, string, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	// Parse the tool path
	parts := strings.Split(toolPath, ".")
	if len(parts) == 0 {
		return nil, "", fmt.Errorf("invalid tool path: %s", toolPath)
	}

	var foundTool *ToolDefinition

	// Strategy 1: Check if the full path is a node, and look for a tool with the same name as the last part
	// e.g., "everything.echo" -> check node "everything.echo" for tool "echo"
	lastPart := parts[len(parts)-1]
	if node, exists := h.nodes[toolPath]; exists {
		if tool, ok := node.Tools[lastPart]; ok {
			foundTool = tool
		}
	}

	// Strategy 2: Try to find the tool by progressively trying longer paths
	// e.g., for "coding_tools.serena.search.find_symbol":
	// - Try "coding_tools.serena.search" with tool "find_symbol"
	// - Then "coding_tools.serena" with tool "find_symbol"
	// - Then "coding_tools" with tool "find_symbol"
	// - Finally "" (root) with tool "find_symbol"
	if foundTool == nil {
		// Start from longest path and work backwards
		for i := len(parts) - 1; i >= 0; i-- {
			var categoryPath string
			var toolName string

			if i == 0 {
				// Single part or trying root
				categoryPath = ""
				toolName = parts[0]
			} else {
				categoryPath = strings.Join(parts[:i], ".")
				toolName = parts[len(parts)-1]
			}

			if node, exists := h.nodes[categoryPath]; exists {
				// Check if this node has the tool
				if tool, ok := node.Tools[toolName]; ok {
					foundTool = tool
					break
				}
			}
		}
	}

	if foundTool == nil {
		return nil, "", fmt.Errorf("tool not found: %s", toolPath)
	}

	// Return the tool and its server name (from the tool-level server field)
	return foundTool, foundTool.Server, nil
}

// HandleExecuteTool handles the execute_tool meta-tool
func (h *Hierarchy) HandleExecuteTool(ctx context.Context, registry *ServerRegistry, toolPath string, arguments map[string]interface{}) (*mcp.CallToolResult, error) {
	// Resolve the tool path to get tool definition and server name
	toolDef, serverName, err := h.ResolveToolPath(toolPath)
	if err != nil {
		log.Printf("Tool call failed: hierarchy_path=%s, status=resolve_error, error=%v", toolPath, err)
		return nil, err
	}

	if serverName == "" {
		log.Printf("Tool call failed: hierarchy_path=%s, status=no_server_configured", toolPath)
		return nil, fmt.Errorf("no MCP server configured for tool: %s", toolPath)
	}

	// Get or load the MCP client for this server
	client, err := registry.GetOrLoadServer(ctx, serverName)
	if err != nil {
		log.Printf("Tool call failed: hierarchy_path=%s, server=%s, status=load_error, error=%v", toolPath, serverName, err)
		return nil, fmt.Errorf("failed to get MCP client: %w", err)
	}

	// Use the mapped tool name
	actualToolName := toolDef.MapsTo
	if actualToolName == "" {
		actualToolName = strings.Split(toolPath, ".")[len(strings.Split(toolPath, "."))-1]
	}

	if coerced, notes := coerceArguments(arguments, toolDef.InputSchema); len(notes) > 0 {
		log.Printf("Coerced arguments to match schema: hierarchy_path=%s, changes=[%s]", toolPath, strings.Join(notes, "; "))
		arguments = coerced
	}

	log.Printf("Executing tool: hierarchy_path=%s, server=%s, tool=%s", toolPath, serverName, actualToolName)
	start := time.Now()
	argsJSON, marshalArgsErr := json.Marshal(arguments)
	if marshalArgsErr != nil {
		argsJSON = []byte(fmt.Sprintf("<failed to marshal arguments: %v>", marshalArgsErr))
	}

	// Create a context with 30-second timeout for tool execution
	// (increased from 15s to account for queuing time when serializing requests)
	// Note: We create the timeout BEFORE acquiring the lock to enforce a total deadline
	// for the operation. If we waited for the lock first, a client could hang indefinitely.
	toolCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Serialize tool calls to the same server to prevent concurrent stdio access.
	// Stdio is a single-channel transport that cannot handle interleaved messages.
	// See: https://github.com/voicetreelab/lazy-mcp/issues/8
	debugEnabled := registry.debugEnabled(serverName)
	waitStart := time.Now()
	mutex := registry.GetClientMutex(serverName)
	mutex.Lock()
	defer mutex.Unlock()
	queueWait := time.Since(waitStart)

	// Call the tool on the actual MCP server
	callRequest := mcp.CallToolRequest{}
	callRequest.Params.Name = actualToolName
	callRequest.Params.Arguments = arguments

	callStart := time.Now()
	result, err := client.GetClient().CallTool(toolCtx, callRequest)
	if err != nil {
		if debugEnabled {
			log.Printf("Tool call completed: hierarchy_path=%s, server=%s, tool=%s, status=transport_error, queue_wait=%s, call_duration=%s, arguments=%s, error=%v",
				toolPath, serverName, actualToolName, queueWait, time.Since(callStart), argsJSON, err)
		} else {
			log.Printf("Tool call completed: hierarchy_path=%s, server=%s, tool=%s, status=transport_error, duration=%s, arguments=%s, error=%v",
				toolPath, serverName, actualToolName, time.Since(start), argsJSON, err)
		}
		registry.EvictServer(serverName, err.Error())
		// Include inputSchema in error message to help LLMs self-correct parameter mistakes
		if toolDef.InputSchema != nil {
			schemaJSON, marshalErr := json.MarshalIndent(toolDef.InputSchema, "", "  ")
			if marshalErr == nil {
				return nil, fmt.Errorf("failed to call tool %s: %w\n\nExpected inputSchema:\n%s", actualToolName, err, string(schemaJSON))
			}
		}
		return nil, fmt.Errorf("failed to call tool %s: %w", actualToolName, err)
	}

	status := "ok"
	if result != nil && result.IsError {
		status = "tool_error"
	}
	if debugEnabled {
		log.Printf("Tool call completed: hierarchy_path=%s, server=%s, tool=%s, status=%s, queue_wait=%s, call_duration=%s, arguments=%s",
			toolPath, serverName, actualToolName, status, queueWait, time.Since(callStart), argsJSON)
	} else {
		log.Printf("Tool call completed: hierarchy_path=%s, server=%s, tool=%s, status=%s, duration=%s, arguments=%s",
			toolPath, serverName, actualToolName, status, time.Since(start), argsJSON)
	}

	// Check if result has IsError set - append schema to help LLMs self-correct
	if result != nil && result.IsError && toolDef.InputSchema != nil && len(result.Content) > 0 {
		schemaJSON, marshalErr := json.MarshalIndent(toolDef.InputSchema, "", "  ")
		if marshalErr == nil {
			// Append schema to the first text content item
			// Note: TextContent is a value type, so we modify the copy and assign it back to the slice
			if textContent, ok := result.Content[0].(mcp.TextContent); ok {
				textContent.Text += fmt.Sprintf("\n\nExpected inputSchema:\n%s", string(schemaJSON))
				result.Content[0] = textContent
			}
		}
	}
	return result, nil
}

// findActivation returns the ActivationConfig declared on the nearest ancestor
// node of path (checking path itself, then progressively shorter dotted
// prefixes), or nil if none of them declare one.
func (h *Hierarchy) findActivation(path string) *ActivationConfig {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if path == "" {
		if node, ok := h.nodes[""]; ok {
			return node.Activation
		}
		return nil
	}

	parts := strings.Split(path, ".")
	for i := len(parts); i >= 1; i-- {
		prefix := strings.Join(parts[:i], ".")
		if node, ok := h.nodes[prefix]; ok && node.Activation != nil {
			return node.Activation
		}
	}
	return nil
}

// activateCategories runs an ActivationConfig's downstream activation sequence
// at most once per server: it calls the server's own activation tool for every
// declared category over its persistent client connection, re-lists the
// server's tools, and merges any newly-visible ones into the hierarchy as flat
// leaves under "<server>.<toolName>" (matching this codebase's existing flat
// naming convention, e.g. structure/gitlab_po/*.json). Subsequent calls for a
// server that has already been activated are no-ops.
func (h *Hierarchy) activateCategories(ctx context.Context, registry *ServerRegistry, activation *ActivationConfig) error {
	h.mu.Lock()
	if h.activated == nil {
		h.activated = make(map[string]bool)
	}
	if h.activated[activation.Server] {
		h.mu.Unlock()
		return nil
	}
	h.activated[activation.Server] = true
	h.mu.Unlock()

	mcpClient, err := registry.GetOrLoadServer(ctx, activation.Server)
	if err != nil {
		return fmt.Errorf("activation: failed to get MCP client for server %s: %w", activation.Server, err)
	}

	// Serialize with regular tool calls to the same server (see HandleExecuteTool).
	mutex := registry.GetClientMutex(activation.Server)
	mutex.Lock()
	defer mutex.Unlock()

	for _, category := range activation.Categories {
		activateRequest := mcp.CallToolRequest{}
		activateRequest.Params.Name = activation.Tool
		activateRequest.Params.Arguments = map[string]interface{}{activation.Param: category}
		if _, callErr := mcpClient.GetClient().CallTool(ctx, activateRequest); callErr != nil {
			log.Printf("activation: failed to activate category %q on server %s: %v", category, activation.Server, callErr)
		}
	}

	listResult, err := mcpClient.GetClient().ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return fmt.Errorf("activation: failed to list tools on server %s after activation: %w", activation.Server, err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, tool := range listResult.Tools {
		toolPath := activation.Server + "." + tool.Name
		if _, exists := h.nodes[toolPath]; exists {
			continue // already known from the static snapshot
		}

		var schemaMap map[string]interface{}
		if schemaJSON, mErr := json.Marshal(tool.InputSchema); mErr == nil {
			_ = json.Unmarshal(schemaJSON, &schemaMap)
		}

		h.nodes[toolPath] = &HierarchyNode{
			Tools: map[string]*ToolDefinition{
				tool.Name: {
					Description: tool.Description,
					MapsTo:      tool.Name,
					Server:      activation.Server,
					InputSchema: schemaMap,
				},
			},
		}
		log.Printf("activation: merged newly-activated tool into hierarchy: %s", toolPath)
	}
	return nil
}

// HandleGetToolsInCategoryDynamic behaves like HandleGetToolsInCategory, but on
// a "category not found" miss it checks whether the nearest ancestor category
// declares an ActivationConfig (see ActivationConfig's doc comment) and, if so,
// runs that activation sequence once and retries. This is what lets
// get_tools_in_category see categories a downstream server only reveals after
// its own runtime activation call.
func (h *Hierarchy) HandleGetToolsInCategoryDynamic(ctx context.Context, registry *ServerRegistry, path string) (map[string]interface{}, error) {
	if _, err := h.HandleGetToolsInCategory(path); err != nil {
		if activation := h.findActivation(normalizeCategoryPath(path)); activation != nil {
			if actErr := h.activateCategories(ctx, registry, activation); actErr != nil {
				log.Printf("activation: %v", actErr)
			}
		}
	}
	return h.HandleGetToolsInCategory(path)
}

// HandleExecuteToolDynamic behaves like HandleExecuteTool, but first probes
// resolution with the pure, side-effect-free ResolveToolPath. If that misses
// and an ancestor category declares an ActivationConfig, it runs the
// activation sequence once before delegating to HandleExecuteTool exactly
// once — so a tool that was merely gated behind a downstream activation call
// gets called exactly once, never twice, regardless of whether activation was
// needed.
func (h *Hierarchy) HandleExecuteToolDynamic(ctx context.Context, registry *ServerRegistry, toolPath string, arguments map[string]interface{}) (*mcp.CallToolResult, error) {
	if _, _, err := h.ResolveToolPath(toolPath); err != nil {
		if activation := h.findActivation(toolPath); activation != nil {
			if actErr := h.activateCategories(ctx, registry, activation); actErr != nil {
				log.Printf("activation: %v", actErr)
			}
		}
	}
	return h.HandleExecuteTool(ctx, registry, toolPath, arguments)
}

// serverStartupTimeout bounds spawning and handshaking a downstream MCP
// server's client on first use. Without this, a hung/failed stdio process
// spawn (e.g. a slow "npx -y ..." install) or a lost Initialize response
// (mark3labs/mcp-go's stdio transport silently drops unmatched responses)
// would block loadServer forever. It is longer than the 30s tool-call
// timeout in HandleExecuteTool because first-time process spawns can be
// much slower than a warm tool call.
const serverStartupTimeout = 60 * time.Second

// restartCooldown bounds how soon a server can be respawned after being
// evicted (crash/hang). Without this, a permanently broken server config
// (e.g. a wrong command) would hot-loop a fresh 60s spawn attempt on every
// single execute_tool call against it.
const restartCooldown = 10 * time.Second

// ServerRegistry manages MCP client connections
type ServerRegistry struct {
	clients       map[string]*client.Client
	clientMutex   map[string]*sync.Mutex   // Per-client mutex for serializing tool calls
	loading       map[string]chan struct{} // per-server in-flight load marker, closed when the load completes
	serverConfigs map[string]*config.MCPClientConfigV2
	lastLoadErr   map[string]error     // most recent loadServer failure per server, surfaced to concurrent waiters
	evictedAt     map[string]time.Time // when a server was last evicted (crash/hang), used for restartCooldown
	mu            sync.RWMutex

	// ctx outlives any single request and is cancelled by Close. Downstream
	// sessions and their health probes run under it: transports tie a session
	// to the context handed to Start, so a per-request context would tear the
	// session down as soon as the request that loaded it returned.
	ctx    context.Context
	cancel context.CancelFunc
}

// NewServerRegistry creates a new server registry with server configurations
func NewServerRegistry(serverConfigs map[string]*config.MCPClientConfigV2) *ServerRegistry {
	ctx, cancel := context.WithCancel(context.Background())
	return &ServerRegistry{
		ctx:           ctx,
		cancel:        cancel,
		clients:       make(map[string]*client.Client),
		clientMutex:   make(map[string]*sync.Mutex),
		loading:       make(map[string]chan struct{}),
		serverConfigs: serverConfigs,
		lastLoadErr:   make(map[string]error),
		evictedAt:     make(map[string]time.Time),
	}
}

// debugEnabled reports whether debugLogging is enabled for serverName
// (falling back to the proxy-level default via the config.Load fallback
// loop, same as logEnabled/lazyLoad).
func (r *ServerRegistry) debugEnabled(serverName string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cfg, exists := r.serverConfigs[serverName]
	if !exists || cfg.Options == nil {
		return false
	}
	return cfg.Options.DebugLogging.OrElse(false)
}

// EvictServer removes serverName's cached client so the next GetOrLoadServer
// call spawns a fresh process, and closes the removed client with a bounded
// timeout (force-killing it if it doesn't shut down gracefully in time).
// Used to recover from a downstream server that crashed or is wedged.
func (r *ServerRegistry) EvictServer(serverName, reason string) {
	r.mu.Lock()
	c, exists := r.clients[serverName]
	if exists {
		delete(r.clients, serverName)
	}
	r.evictedAt[serverName] = time.Now()
	r.mu.Unlock()

	if !exists {
		return
	}

	log.Printf("Evicting MCP client for server=%s reason=%q", serverName, reason)
	if err := c.CloseWithTimeout(5 * time.Second); err != nil {
		log.Printf("Force-killed MCP client for server=%s after close timeout: %v", serverName, err)
	}
}

// GetClientMutex returns a mutex for the given server, creating one if needed.
// This mutex serializes tool calls to prevent concurrent stdio access.
// Note: This map grows with the number of unique servers accessed. Since the set of
// servers is bounded by the configuration/hierarchy, this is not a memory leak.
//
// The common case (mutex already exists) only takes a read lock, so a warm
// server's execute_tool call is never blocked behind r.mu by an unrelated
// server's slow first-time load in GetOrLoadServer.
func (r *ServerRegistry) GetClientMutex(serverName string) *sync.Mutex {
	r.mu.RLock()
	if m, exists := r.clientMutex[serverName]; exists {
		r.mu.RUnlock()
		return m
	}
	r.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()

	if m, exists := r.clientMutex[serverName]; exists {
		return m
	}

	m := &sync.Mutex{}
	r.clientMutex[serverName] = m
	return m
}

// GetOrLoadServer gets an existing client or creates and initializes a new one.
// This implements lazy loading - servers are only started when first accessed.
//
// r.mu is never held across the slow spawn/handshake work (see loadServer) -
// only to check/update the clients and loading maps. This ensures a hung or
// slow load for one server can never block GetClientMutex/GetOrLoadServer
// calls for any other server (see https://github.com/voicetreelab/lazy-mcp/issues/8
// for the original per-server serialization this must not regress).
func (r *ServerRegistry) GetOrLoadServer(ctx context.Context, serverName string) (*client.Client, error) {
	r.mu.RLock()
	if c, exists := r.clients[serverName]; exists {
		r.mu.RUnlock()
		return c, nil
	}
	r.mu.RUnlock()

	r.mu.Lock()
	if c, exists := r.clients[serverName]; exists {
		r.mu.Unlock()
		return c, nil
	}

	if ch, loadInProgress := r.loading[serverName]; loadInProgress {
		// Another goroutine is already loading this server: wait for it to
		// finish without holding r.mu, so other servers are never blocked.
		r.mu.Unlock()
		<-ch
		r.mu.RLock()
		c, exists := r.clients[serverName]
		loadErr := r.lastLoadErr[serverName]
		r.mu.RUnlock()
		if !exists {
			if loadErr != nil {
				return nil, fmt.Errorf("failed to load MCP client for server %s: %w", serverName, loadErr)
			}
			return nil, fmt.Errorf("failed to load MCP client for server: %s", serverName)
		}
		return c, nil
	}

	if evictedAt, wasEvicted := r.evictedAt[serverName]; wasEvicted {
		if remaining := restartCooldown - time.Since(evictedAt); remaining > 0 {
			r.mu.Unlock()
			return nil, fmt.Errorf("server %s recently crashed/timed out, retrying in cooldown (%s left)", serverName, remaining.Round(time.Millisecond))
		}
	}

	ch := make(chan struct{})
	r.loading[serverName] = ch
	r.mu.Unlock()

	mcpClient, loadErr := r.loadServer(ctx, serverName)

	r.mu.Lock()
	if loadErr == nil {
		r.clients[serverName] = mcpClient
		delete(r.lastLoadErr, serverName)
	} else {
		r.lastLoadErr[serverName] = loadErr
	}
	delete(r.loading, serverName)
	r.mu.Unlock()
	close(ch)

	if loadErr != nil {
		return nil, loadErr
	}

	// Keep the downstream under health watch for as long as the registry lives.
	mcpClient.StartPing(r.ctx)

	return mcpClient, nil
}

// loadServer creates, starts and initializes a new MCP client for serverName,
// bounded by serverStartupTimeout so it can never hang forever. Must be
// called without holding r.mu. On any failure it closes any partially
// started client so the subprocess isn't leaked, and returns an error
// without caching anything, so the next GetOrLoadServer call retries a
// fresh spawn instead of being permanently stuck.
func (r *ServerRegistry) loadServer(ctx context.Context, serverName string) (*client.Client, error) {
	r.mu.RLock()
	cfg, exists := r.serverConfigs[serverName]
	r.mu.RUnlock()
	if !exists {
		return nil, fmt.Errorf("server config not found: %s", serverName)
	}

	debugEnabled := cfg.Options != nil && cfg.Options.DebugLogging.OrElse(false)
	loadStart := time.Now()

	spawnStart := time.Now()
	mcpClient, err := client.NewMCPClient(serverName, cfg)
	if debugEnabled {
		log.Printf("Server load phase=spawn server=%s duration=%s error=%v", serverName, time.Since(spawnStart), err)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create MCP client: %w", err)
	}

	// Start and initialize. The client watches a stdio subprocess's stderr, so
	// a crash during startup aborts the handshake at once and is reported with
	// the server's own diagnostics instead of waiting out the timeout.
	connectStart := time.Now()
	connectErr := r.connect(ctx, mcpClient)
	if debugEnabled {
		log.Printf("Server load phase=connect server=%s duration=%s error=%v", serverName, time.Since(connectStart), connectErr)
	}
	if connectErr != nil {
		_ = mcpClient.CloseWithTimeout(5 * time.Second)
		return nil, fmt.Errorf("failed to connect MCP client: %w", connectErr)
	}

	log.Printf("Created and initialized MCP client for server: %s, duration=%s", serverName, time.Since(loadStart))
	return mcpClient, nil
}

// connect runs c's handshake, bounded by serverStartupTimeout and ctx. The
// handshake itself runs under the registry's long-lived context (see ctx), so
// a bound here has to be enforced by abandoning it: the caller then closes c,
// which unblocks the in-flight handshake.
func (r *ServerRegistry) connect(ctx context.Context, c *client.Client) error {
	done := make(chan error, 1)
	go func() {
		done <- c.Connect(r.ctx, mcp.Implementation{Name: "mcp-proxy-recursive"})
	}()

	timer := time.NewTimer(serverStartupTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return fmt.Errorf("timed out after %s", serverStartupTimeout)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close closes all clients in the registry
func (r *ServerRegistry) Close() {
	r.cancel()
	r.mu.Lock()
	defer r.mu.Unlock()

	for name, client := range r.clients {
		log.Printf("Closing MCP client: %s", name)
		_ = client.Close()
	}

	// Clear the clients, mutex and loading maps
	r.clients = make(map[string]*client.Client)
	r.clientMutex = make(map[string]*sync.Mutex)
	r.loading = make(map[string]chan struct{})
}
