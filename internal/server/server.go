package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/voicetreelab/lazy-mcp/internal/config"
	"github.com/voicetreelab/lazy-mcp/internal/hierarchy"
)

type MiddlewareFunc func(http.Handler) http.Handler

func chainMiddleware(h http.Handler, middlewares ...MiddlewareFunc) http.Handler {
	for _, mw := range middlewares {
		h = mw(h)
	}
	return h
}

func newAuthMiddleware(tokens []string) MiddlewareFunc {
	tokenSet := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		tokenSet[token] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(tokens) != 0 {
				token := r.Header.Get("Authorization")
				token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
				if token == "" {
					http.Error(w, "Unauthorized", http.StatusUnauthorized)
					return
				}
				if _, ok := tokenSet[token]; !ok {
					http.Error(w, "Unauthorized", http.StatusUnauthorized)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func loggerMiddleware(prefix string) MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log.Printf("<%s> Request [%s] %s", prefix, r.Method, r.URL.Path)
			next.ServeHTTP(w, r)
		})
	}
}

func recoverMiddleware(prefix string) MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if err := recover(); err != nil {
					log.Printf("<%s> Recovered from panic: %v", prefix, err)
					http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// newMetaToolServer builds the MCP server that fronts the hierarchy: exactly
// two meta-tools, get_tools_in_category and execute_tool, backed by a registry
// that starts downstream servers on first use. The returned cleanup closes the
// registry and every downstream client.
func newMetaToolServer(cfg *config.Config) (*server.MCPServer, func(), error) {
	log.Printf("Loading hierarchy from %s", cfg.McpProxy.HierarchyPath)
	h, err := hierarchy.LoadHierarchy(cfg.McpProxy.HierarchyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load hierarchy: %w", err)
	}

	// Lazy-loaded MCP clients: a downstream starts on first use.
	registry := hierarchy.NewServerRegistry(cfg.McpServers)

	serverOpts := []server.ServerOption{
		server.WithRecovery(),
	}
	if cfg.McpProxy.Options != nil && cfg.McpProxy.Options.LogEnabled.OrElse(false) {
		serverOpts = append(serverOpts, server.WithLogging())
	}
	mcpServer := server.NewMCPServer(
		cfg.McpProxy.Name,
		cfg.McpProxy.Version,
		serverOpts...,
	)

	// Register get_tools_in_category meta-tool
	// Build description from root overview
	description := "You have MCP tools hidden within categories. You MUST use get_tools_in_category to learn more about what available tools you have within these categories. Returns children categories, and tools at the specified path. Call initially with an empty string to get root categories."

	// Get root node and use its overview
	if rootNode := h.GetRootNode(); rootNode != nil && rootNode.Overview != "" {
		description += fmt.Sprintf("\n\n%s", rootNode.Overview)
	}

	getToolsInCategoryTool := mcp.Tool{
		Name:        "get_tools_in_category",
		Description: description,
		InputSchema: mcp.ToolInputSchema{
			Type: "object",
			Properties: map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "Category path using dot notation (e.g., 'coding_tools' or 'coding_tools.serena.search'). Use empty string or '/' for root.",
				},
			},
			Required: []string{"path"},
		},
	}

	mcpServer.AddTool(getToolsInCategoryTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		path := ""
		if request.Params.Arguments != nil {
			if argsMap, ok := request.Params.Arguments.(map[string]interface{}); ok {
				if pathVal, ok := argsMap["path"].(string); ok {
					path = pathVal
				}
			}
		}

		response, err := h.HandleGetToolsInCategoryDynamic(ctx, registry, path)
		if err != nil {
			return nil, err
		}

		jsonBytes, err := json.MarshalIndent(response, "", "  ")
		if err != nil {
			return nil, err
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				mcp.NewTextContent(string(jsonBytes)),
			},
		}, nil
	})

	// Register execute_tool meta-tool
	executeToolTool := mcp.Tool{
		Name:        "execute_tool",
		Description: "Execute a tool by its full path. Automatically proxies the request to the appropriate MCP server.",
		InputSchema: mcp.ToolInputSchema{
			Type: "object",
			Properties: map[string]interface{}{
				"tool_path": map[string]interface{}{
					"type":        "string",
					"description": "Full tool path using dot notation (e.g., 'coding_tools.serena.search.search_symbol') or just tool name if unique",
				},
				"arguments": map[string]interface{}{
					"type":                 "object",
					"description":          "Arguments to pass to the tool",
					"additionalProperties": true,
				},
			},
			Required: []string{"tool_path", "arguments"},
		},
	}

	mcpServer.AddTool(executeToolTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		toolPath := ""
		arguments := make(map[string]interface{})

		if request.Params.Arguments != nil {
			if argsMap, ok := request.Params.Arguments.(map[string]interface{}); ok {
				if pathVal, ok := argsMap["tool_path"].(string); ok {
					toolPath = pathVal
				}
				if argsVal, ok := argsMap["arguments"].(map[string]interface{}); ok {
					arguments = argsVal
				}
			}
		}

		if toolPath == "" {
			return nil, fmt.Errorf("tool_path is required")
		}

		return h.HandleExecuteToolDynamic(ctx, registry, toolPath, arguments)
	})

	return mcpServer, registry.Close, nil
}

// StartStdioServer serves the hierarchy over stdin/stdout.
func StartStdioServer(cfg *config.Config) error {
	mcpServer, cleanup, err := newMetaToolServer(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	log.Printf("Starting hierarchical MCP proxy (stdio server)")
	return server.ServeStdio(mcpServer)
}

// StartHTTPServer serves the hierarchy over SSE or streamable HTTP.
func StartHTTPServer(cfg *config.Config) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mcpServer, cleanup, err := newMetaToolServer(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	// Set up HTTP handler (SSE or Streamable)
	var handler http.Handler
	switch cfg.McpProxy.Type {
	case config.MCPServerTypeSSE:
		handler = server.NewSSEServer(
			mcpServer,
			server.WithStaticBasePath(""),
			server.WithBaseURL(cfg.McpProxy.BaseURL),
		)
	case config.MCPServerTypeStreamable:
		handler = server.NewStreamableHTTPServer(
			mcpServer,
			server.WithStateLess(true),
		)
	default:
		return fmt.Errorf("unknown server type: %s", cfg.McpProxy.Type)
	}

	// Apply middleware
	middlewares := make([]MiddlewareFunc, 0)
	middlewares = append(middlewares, recoverMiddleware("mcp-proxy"))
	if cfg.McpProxy.Options != nil && cfg.McpProxy.Options.LogEnabled.OrElse(false) {
		middlewares = append(middlewares, loggerMiddleware("mcp-proxy"))
	}
	if cfg.McpProxy.Options != nil && len(cfg.McpProxy.Options.AuthTokens) > 0 {
		middlewares = append(middlewares, newAuthMiddleware(cfg.McpProxy.Options.AuthTokens))
	}
	handler = chainMiddleware(handler, middlewares...)

	// Start HTTP server
	httpMux := http.NewServeMux()
	httpMux.Handle("/", handler)

	httpServer := &http.Server{
		Addr:    cfg.McpProxy.Addr,
		Handler: httpMux,
	}

	go func() {
		log.Printf("Starting hierarchical MCP proxy (%s server)", cfg.McpProxy.Type)
		log.Printf("%s server listening on %s", cfg.McpProxy.Type, cfg.McpProxy.Addr)
		hErr := httpServer.ListenAndServe()
		if hErr != nil && !errors.Is(hErr, http.ErrServerClosed) {
			log.Fatalf("Failed to start server: %v", hErr)
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	<-sigChan
	log.Println("Shutdown signal received")

	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 5*time.Second)
	defer shutdownCancel()

	err = httpServer.Shutdown(shutdownCtx)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
