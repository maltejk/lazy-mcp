package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/voicetreelab/lazy-mcp/internal/config"
)

// clientHealth is the last known state of a downstream connection.
//
// A client that never connected stays healthUnknown and is left out of the
// readiness report: a server that is misconfigured or down at startup must not
// keep the whole proxy out of rotation, since the other servers still work. One
// that connected and later broke does report unhealthy, because that is a
// regression a load balancer should route around.
type clientHealth int32

const (
	healthUnknown clientHealth = iota
	healthOK
	healthFailed
)

// BuildVersion is reported to downstream servers during OAuth/doctor handshakes.
// main sets it from its ldflags-injected value.
var BuildVersion = "dev"

type Client struct {
	name            string
	needPing        bool
	needManualStart bool
	options         *config.OptionsV2
	health          atomic.Int32
	// requestTimeout bounds one forwarded request. It is only set for stdio
	// downstreams: the sse and streamable-http clients carry their timeout
	// inside the transport, but the stdio transport has no equivalent, so the
	// bound has to be applied to the context of each call instead.
	requestTimeout time.Duration

	// clientConf is the parsed transport config. It is kept so a dropped
	// downstream can be rebuilt from scratch instead of reusing a dead client.
	clientConf any

	// mu guards client. Forwarded calls resolve the live connection through
	// getClient at request time, so reconnecting can swap it under them.
	mu     sync.RWMutex
	client *client.Client
	// cmd is the stdio subprocess behind client, captured at spawn time so
	// Kill can force-terminate it: mcp-go's stdio Close only closes stdin and
	// then blocks on cmd.Wait(), never sending a signal. Guarded by mu.
	cmd *exec.Cmd
	// stderr watches the stdio subprocess behind client. Guarded by mu.
	stderr *stderrWatcher

	// connectMu serializes connect attempts: the startup retry loop and the
	// ping-driven reconnect must never build a transport at the same time.
	connectMu sync.Mutex
	// hasConnected is false until the first connect succeeds. Until then the
	// transport built in NewMCPClient is reused (stdio spawns its subprocess
	// there); afterwards every attempt rebuilds, so a dead one is never reused.
	hasConnected bool
	closed       atomic.Bool

	// remembered from the last AddToMCPServer so the ping task can reconnect.
	clientInfo mcp.Implementation
	mcpServer  *server.MCPServer
	pingOnce   sync.Once

	// activateMu serializes activation of a lazy-loaded downstream, and
	// activated records that its real catalog is now mounted.
	activateMu sync.Mutex
	activated  atomic.Bool
}

func (c *Client) Health() clientHealth {
	return clientHealth(c.health.Load())
}

const acceptEncodingHeader = "Accept-Encoding"

// mcpHTTPHeaders returns a copy of headers that explicitly opts out of response
// compression. Some MCP servers otherwise return gzip data that reaches the JSON
// decoder without being decompressed by Go's HTTP transport.
func mcpHTTPHeaders(headers map[string]string) map[string]string {
	result := make(map[string]string, len(headers)+1)
	for key, value := range headers {
		if strings.EqualFold(key, acceptEncodingHeader) {
			continue
		}
		result[key] = value
	}
	result[acceptEncodingHeader] = "identity"
	return result
}

// sseClientOptions and streamableClientOptions keep the plain and OAuth
// variants of each transport configured identically.
func sseClientOptions(conf *config.SSEMCPClientConfig) []transport.ClientOption {
	options := []transport.ClientOption{client.WithHeaders(mcpHTTPHeaders(conf.Headers))}
	if conf.Timeout > 0 {
		options = append(options, transport.WithResponseTimeout(time.Duration(conf.Timeout)))
	}
	return options
}

func streamableClientOptions(conf *config.StreamableMCPClientConfig) []transport.StreamableHTTPCOption {
	options := []transport.StreamableHTTPCOption{transport.WithHTTPHeaders(mcpHTTPHeaders(conf.Headers))}
	if conf.Timeout > 0 {
		options = append(options, transport.WithHTTPTimeout(time.Duration(conf.Timeout)))
	}
	return options
}

func NewMCPClient(name string, conf *config.MCPClientConfigV2) (*Client, error) {
	clientInfo, pErr := config.ParseMCPClientConfigV2(conf)
	if pErr != nil {
		return nil, pErr
	}
	c := &Client{
		name:       name,
		options:    conf.Options,
		clientConf: clientInfo,
	}
	switch v := clientInfo.(type) {
	case *config.StdioMCPClientConfig:
		// Stdio servers are pinged too: a crashed subprocess is the most
		// common way a downstream disappears at runtime.
		c.needPing = true
		c.requestTimeout = time.Duration(v.Timeout)
	case *config.SSEMCPClientConfig, *config.StreamableMCPClientConfig:
		c.needPing = true
		c.needManualStart = true
	}
	// The first transport is built here: for stdio that spawns the subprocess,
	// so a missing command fails at startup rather than on first use.
	raw, err := c.buildRawClient()
	if err != nil {
		return nil, err
	}
	c.client = raw
	return c, nil
}

// buildRawClient creates a fresh underlying transport from the stored config.
// It is called once at construction and again on every reconnect, so a dead
// backend (closed Obsidian, crashed stdio server) is replaced rather than
// reused.
func (c *Client) buildRawClient() (*client.Client, error) {
	switch v := c.clientConf.(type) {
	case *config.StdioMCPClientConfig:
		envs := make([]string, 0, len(v.Env))
		for kk, vv := range v.Env {
			envs = append(envs, fmt.Sprintf("%s=%s", kk, vv))
		}
		raw, err := client.NewStdioMCPClientWithOptions(v.Command, envs, v.Args,
			transport.WithCommandFunc(func(ctx context.Context, command string, env []string, args []string) (*exec.Cmd, error) {
				// Mirrors mcp-go's default spawn (exec.CommandContext plus the
				// merged environment), but remembers the process for Kill.
				cmd := exec.CommandContext(ctx, command, args...)
				cmd.Env = append(os.Environ(), env...)
				c.mu.Lock()
				c.cmd = cmd
				c.mu.Unlock()
				return cmd, nil
			}),
		)
		if err != nil {
			return nil, err
		}
		watcher := drainStderr(c.name, raw)
		c.mu.Lock()
		c.stderr = watcher
		c.mu.Unlock()
		return raw, nil
	case *config.SSEMCPClientConfig:
		options := sseClientOptions(v)
		if v.OAuth != nil {
			oc, err := buildOAuthConfig(c.name, v.OAuth)
			if err != nil {
				return nil, err
			}
			return client.NewOAuthSSEClient(v.URL, oc, options...)
		}
		return client.NewSSEMCPClient(v.URL, options...)
	case *config.StreamableMCPClientConfig:
		options := streamableClientOptions(v)
		if v.OAuth != nil {
			oc, err := buildOAuthConfig(c.name, v.OAuth)
			if err != nil {
				return nil, err
			}
			return client.NewOAuthStreamableHttpClient(v.URL, oc, options...)
		}
		return client.NewStreamableHttpClient(v.URL, options...)
	}
	return nil, errors.New("invalid client type")
}

// getClient returns the live transport, or nil before the first connection.
// Forwarded calls and the health probe go through it, so reconnecting can swap
// the transport underneath them.
func (c *Client) getClient() *client.Client {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.client
}

// connect builds a transport, initializes it, swaps it in (closing the previous
// one), then (re)registers the downstream's tools/prompts/resources. Tool
// handlers resolve the live client via getClient at call time, so the swap is
// transparent to in-flight and future requests.
func (c *Client) connect(ctx context.Context, clientInfo mcp.Implementation, mcpServer *server.MCPServer) error {
	c.connectMu.Lock()
	defer c.connectMu.Unlock()

	if c.closed.Load() {
		return errors.New("client is closed")
	}

	// Reuse the transport built at construction for the very first attempt;
	// after that (or after a failed first attempt, which nils it out) build a
	// fresh one so a dead transport is never reused.
	raw := c.getClient()
	if raw == nil || c.hasConnected {
		var err error
		raw, err = c.buildRawClient()
		if err != nil {
			return err
		}
	}

	c.mu.RLock()
	watcher := c.stderr
	c.mu.RUnlock()

	connected := false
	// Until the swap below succeeds, this transport is not owned by the client
	// and must be closed on the way out.
	defer func() {
		if !connected {
			_ = raw.Close()
		}
	}()

	// Start is given the long-lived context, not a bounded one: mcp-go's SSE
	// transport ties its event stream to the context Start receives, and stdio
	// stores it for request handling, so a deadline there would tear the
	// session down as soon as it elapsed.
	if c.needManualStart {
		if err := raw.Start(ctx); err != nil {
			c.forget(raw)
			return oauthAwareError(c.name, watcher.explain(c.name, err))
		}
	}

	// Initialize is a request/response, so it is bounded: a downstream that
	// accepts the connection and then goes silent must not wedge a retry loop
	// forever. Tool/prompt/resource listing below uses the caller's context,
	// since a large catalog can legitimately take longer than a handshake.
	initCtx, cancelTimeout := context.WithTimeout(ctx, connectTimeout)
	defer cancelTimeout()
	// A stdio downstream that exits during startup closes its stderr. Stop
	// waiting for the handshake then, instead of sitting out connectTimeout.
	initCtx, cancelInit := context.WithCancelCause(initCtx)
	defer cancelInit(nil)
	if watcher != nil {
		go func() {
			select {
			case <-watcher.exited:
				cancelInit(watcher.exitError(c.name))
			case <-initCtx.Done():
			}
		}()
	}

	initRequest := mcp.InitializeRequest{}
	initRequest.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initRequest.Params.ClientInfo = clientInfo
	initRequest.Params.Capabilities = mcp.ClientCapabilities{
		Experimental: make(map[string]any),
		Roots:        nil,
		Sampling:     nil,
	}
	if _, err := raw.Initialize(initCtx, initRequest); err != nil {
		c.forget(raw)
		return oauthAwareError(c.name, watcher.explain(c.name, err))
	}

	c.mu.Lock()
	old := c.client
	c.client = raw
	c.mu.Unlock()
	connected = true
	// The transport is established now, so any later attempt must build a new
	// one rather than re-initializing this one.
	c.hasConnected = true
	if old != nil && old != raw {
		_ = old.Close()
	}
	// Close does not hold connectMu, so it may have run while this transport was
	// being built - it would have closed the previous one, not this. Close this
	// one here rather than leak it (a stdio transport owns a subprocess).
	if c.closed.Load() {
		_ = raw.Close()
		return errors.New("client is closed")
	}
	slog.Info("Successfully initialized MCP client", "client", c.name)

	// A caller that mounts no catalog (the hierarchy router lists and calls
	// tools itself) is done once the handshake succeeded.
	if mcpServer == nil {
		c.health.Store(int32(healthOK))
		return nil
	}

	// Bound the catalog discovery so a downstream that answers initialize and
	// then goes silent cannot wedge this attempt (and the retry loop with it).
	// The transports use this ctx per request, so it does not tear down the
	// connection the way bounding Start would.
	catalogCtx, cancelCatalog := context.WithTimeout(ctx, catalogTimeout)
	defer cancelCatalog()

	// A lazy-loaded downstream mounts only an activation meta-tool until it is
	// called; once activated, a reconnect re-mounts the real catalog.
	if c.lazyLoadPending() {
		if err := c.registerCatalogSafely(func() error {
			return c.registerMetaTool(catalogCtx, mcpServer)
		}); err != nil {
			return err
		}
		c.health.Store(int32(healthOK))
		return nil
	}

	// Catalog registration copies descriptors that a downstream controls, so it
	// runs through the panic-guarded helper below: a future shape this code does
	// not anticipate must cost that backend its own route, not terminate the
	// process that serves every route.
	if err := c.registerCatalogSafely(func() error {
		return c.addToolsToServer(catalogCtx, mcpServer)
	}); err != nil {
		return err
	}
	_ = c.registerCatalogSafely(func() error {
		_ = c.addPromptsToServer(catalogCtx, mcpServer)
		_ = c.addResourcesToServer(catalogCtx, mcpServer)
		_ = c.addResourceTemplatesToServer(catalogCtx, mcpServer)
		return nil
	})

	c.health.Store(int32(healthOK))
	return nil
}

// registerCatalogSafely runs fn and converts a panic caused by
// downstream-supplied catalog data into an error, so a malformed descriptor
// cannot take down the shared process before it serves any route.
func (c *Client) registerCatalogSafely(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("Recovered from panic while registering downstream catalog",
				"client", c.name, "err", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("downstream catalog registration panicked: %v", r)
		}
	}()
	return fn()
}

// forget drops a transport that failed to establish, so the next attempt
// builds a new one instead of reusing a broken one.
func (c *Client) forget(raw *client.Client) {
	c.mu.Lock()
	if c.client == raw {
		c.client = nil
	}
	c.mu.Unlock()
}

func (c *Client) AddToMCPServer(ctx context.Context, clientInfo mcp.Implementation, mcpServer *server.MCPServer) error {
	c.clientInfo = clientInfo
	c.mcpServer = mcpServer

	if err := c.connect(ctx, clientInfo, mcpServer); err != nil {
		return err
	}

	if c.needPing {
		c.pingOnce.Do(func() {
			go c.startPingTask(ctx)
		})
	}
	return nil
}

// reconnect rebuilds a dropped downstream using the details remembered at
// first connect. It is only called from the ping task.
func (c *Client) reconnect(ctx context.Context) error {
	return c.connect(ctx, c.clientInfo, c.mcpServer)
}

// withRequestTimeout bounds ctx by the configured per-request timeout when one
// is set (stdio only), so every forwarded method - not just tool calls - can
// abandon a stalled request instead of occupying the single shared channel.
func (c *Client) withRequestTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.requestTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.requestTimeout)
}

// callTool forwards a tool call to the downstream, bounded by requestTimeout
// when one is configured.
//
// The bound matters most for stdio: a stdio downstream shares one pipe for
// every request, so a call the server accepts but never answers keeps that pipe
// occupied. The keepalive ping then gets no reply either, and after
// pingFailureThreshold probes the client is marked unhealthy and stays there —
// the whole downstream is lost to every caller, not just the one that made the
// wedging call. sse and streamable-http already bound this inside their
// transports, so requestTimeout is left zero for them and the caller's context
// is forwarded unchanged.
func (c *Client) callTool(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	cl := c.getClient()
	if cl == nil {
		return nil, errors.New("downstream is not connected")
	}
	callCtx, cancel := c.withRequestTimeout(ctx)
	defer cancel()
	result, err := cl.CallTool(callCtx, request)
	return result, config.RedactURLCredentials(err)
}

// pingTimeout bounds a single probe, so a downstream that accepts the request
// and then stalls cannot block the health loop until shutdown.
const pingTimeout = 10 * time.Second

// connectTimeout bounds one establish attempt. A downstream that accepts the
// connection and then never completes initialize must not wedge a retry loop.
const connectTimeout = 30 * time.Second

// catalogTimeout bounds the tool/prompt/resource discovery half of one connect
// attempt. The transports carry no timeout of their own for these calls (the
// stdio and streamable-http clients have none by default), so without this a
// downstream that completes initialize and then goes silent mid-listing would
// leave the route unmounted forever and never reach the retry loop. It is far
// looser than connectTimeout because a large catalog is legitimately slow.
//
// It is a variable so tests can shorten it; the e2e suite drives the real binary
// and so cannot, which is why the regression test for this lives in the package.
var catalogTimeout = 60 * time.Second

// probe liveness of a downstream connection. Protocol version 2026-07-28
// removed ping: the stateless protocol core has no connection to keep alive,
// and mcp-go's client answers its Ping with an immediate nil on a modern
// connection, which would make every probe look healthy. A real request
// doubles as the probe there. The failure classification is unchanged: a
// transport error means the connection is broken, while a JSON-RPC error
// response is proof the downstream answered and is therefore alive. ListTools
// is the one call every mounted client already answered during startup, so it
// works for both protocol eras.
func (c *Client) probe(ctx context.Context) error {
	cl := c.getClient()
	if cl == nil {
		return errors.New("downstream is not connected")
	}
	if mcp.IsModernProtocol(cl.ProtocolVersion()) {
		_, err := cl.ListTools(ctx, mcp.ListToolsRequest{})
		return err
	}
	// Ping is deprecated because it is a no-op on modern connections - which is
	// exactly why probe() only reaches it on legacy ones. There is no
	// replacement liveness call for servers older than 2026-07-28.
	return cl.Ping(ctx) //nolint:staticcheck // intentional legacy fallback, see above
}

// pingFailureThreshold is how many probes in a row have to fail before the
// connection counts as broken. One is not enough: a single-threaded downstream
// (the common shape for Python and Node stdio servers) busy with a long tool
// call answers no pings at all, and taking the whole proxy out of rotation for
// that would pull a pod mid-request.
const pingFailureThreshold = 3

// isTransportFailure reports whether err means the connection itself is
// broken. A downstream that answers with a JSON-RPC error is still alive - it
// may simply not implement ping - so only transport errors count as unhealthy.
func isTransportFailure(err error) bool {
	var transportErr *transport.Error
	return errors.As(err, &transportErr)
}

func (c *Client) startPingTask(ctx context.Context) {
	ticker := time.NewTicker(c.options.EffectivePingInterval())
	defer ticker.Stop()

	autoReconnect := c.options.EffectiveAutoReconnect()
	failCount := 0
	for {
		select {
		case <-ctx.Done():
			slog.Debug("Context done, stopping ping", "client", c.name)
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
			err := c.probe(pingCtx)
			cancel()
			if ctx.Err() != nil {
				return
			}
			if err == nil {
				if failCount > 0 {
					slog.Info("MCP health probe recovered", "client", c.name, "failures", failCount)
					failCount = 0
				}
				c.health.Store(int32(healthOK))
				continue
			}
			if !isTransportFailure(err) {
				// A downstream that answers with a JSON-RPC error is alive - it
				// may simply not implement the probe. Only a broken connection
				// counts against it.
				slog.Debug("MCP health probe answered with an error, treating as alive", "client", c.name, "err", config.RedactURLCredentials(err))
				if failCount > 0 {
					slog.Info("MCP health probe recovered", "client", c.name, "failures", failCount)
					failCount = 0
				}
				c.health.Store(int32(healthOK))
				continue
			}

			failCount++
			slog.Warn("MCP health probe failed", "client", c.name, "err", config.RedactURLCredentials(err), "failures", failCount)
			if failCount < pingFailureThreshold {
				continue
			}
			c.health.Store(int32(healthFailed))

			if !autoReconnect {
				continue
			}
			// Rebuild the downstream from scratch. connect() swaps the live
			// transport in, so requests that arrive during the rebuild keep
			// using the old one until the new one is ready.
			rErr := c.reconnect(ctx)
			if rErr != nil {
				slog.Warn("Failed to reconnect downstream", "client", c.name, "err", config.RedactURLCredentials(rErr))
				continue
			}
			slog.Info("Reconnected downstream", "client", c.name, "afterFailures", failCount)
			failCount = 0
			c.health.Store(int32(healthOK))
		}
	}
}

// toolFilterFunc builds the tool exposure predicate for one client.
//
// Behavior is deliberately unchanged from earlier releases so an upgrade cannot
// start hiding tools a deployment relies on: a filter only takes effect when it
// has a non-empty list (so an empty list means "no filtering", including
// mode=allow), and an unrecognized mode skips filtering. Both cases log a
// warning so an inert or over-broad filter is not silent.
func toolFilterFunc(name string, options *config.OptionsV2) func(string) bool {
	if options == nil || options.ToolFilter == nil {
		return func(string) bool { return true }
	}
	filterSet := make(map[string]struct{}, len(options.ToolFilter.List))
	for _, toolName := range options.ToolFilter.List {
		filterSet[toolName] = struct{}{}
	}
	switch mode := config.ToolFilterMode(strings.ToLower(string(options.ToolFilter.Mode))); mode {
	case config.ToolFilterModeAllow:
		if len(filterSet) == 0 {
			slog.Warn("toolFilter mode=allow with an empty list exposes every tool; list the tools to expose, or use mode=block",
				"client", name)
			return func(string) bool { return true }
		}
		return func(toolName string) bool {
			_, inList := filterSet[toolName]
			if !inList {
				slog.Debug("Ignoring tool not in allow list", "client", name, "tool", toolName)
			}
			return inList
		}
	case config.ToolFilterModeBlock:
		return func(toolName string) bool {
			_, inList := filterSet[toolName]
			if inList {
				slog.Debug("Ignoring tool in block list", "client", name, "tool", toolName)
			}
			return !inList
		}
	default:
		// config.ValidateConfig rejects unknown modes, so this only happens for a
		// programmatically built config. Preserve the historical behavior (no
		// filtering) but make it visible.
		slog.Warn("Unknown tool filter mode, skipping tool filter", "client", name, "mode", mode)
		return func(string) bool { return true }
	}
}

func (c *Client) addToolsToServer(ctx context.Context, mcpServer *server.MCPServer) error {
	toolsRequest := mcp.ListToolsRequest{}
	filterFunc := toolFilterFunc(c.name, c.options)

	cl := c.getClient()
	if cl == nil {
		return errors.New("downstream is not connected")
	}
	// Collect first and replace the whole set at the end. AddTool is an upsert,
	// so adding into the existing set would leave a tool the downstream no
	// longer exposes registered after a reconnect (e.g. it was restarted on a
	// smaller tool set). SetTools replaces, which drops the vanished entries.
	tools := make([]server.ServerTool, 0)
	for {
		listed, err := cl.ListTools(ctx, toolsRequest)
		if err != nil {
			return err
		}
		if listed == nil {
			return fmt.Errorf("<%s> ListTools returned nil response without error", c.name)
		}
		if len(listed.Tools) == 0 {
			break
		}
		slog.Debug("Successfully listed tools", "client", c.name, "count", len(listed.Tools))
		for _, tool := range listed.Tools {
			if !filterFunc(tool.Name) {
				continue
			}
			slog.Debug("Adding tool", "client", c.name, "tool", tool.Name)
			// The proxy implements no tasks/* handling (NewMCPServer never
			// enables task capabilities), so a peer-declared execution mode must
			// not select a dispatcher path the route server cannot honour: it
			// would let a plain tools/call be refused, or be accepted as a task
			// whose result can never be read or cancelled. Drop the
			// downstream-supplied execution metadata before republishing.
			tool.Execution = nil
			toolName := tool.Name
			tools = append(tools, server.ServerTool{Tool: tool, Handler: func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				result, err := c.callTool(ctx, request)
				if err != nil {
					slog.Error("Tool call failed", "client", c.name, "tool", toolName, "err", err)
				} else if result != nil && result.IsError {
					slog.Error("Tool call failed", "client", c.name, "tool", toolName, "isError", true)
				}
				return result, err
			}})
		}
		if listed.NextCursor == "" {
			break
		}
		toolsRequest.Params.Cursor = listed.NextCursor
	}
	mcpServer.SetTools(tools...)

	return nil
}

func (c *Client) addPromptsToServer(ctx context.Context, mcpServer *server.MCPServer) error {
	cl := c.getClient()
	if cl == nil {
		return errors.New("downstream is not connected")
	}
	promptsRequest := mcp.ListPromptsRequest{}
	// Collect and replace, so a prompt the downstream dropped is not left
	// registered after a reconnect.
	prompts := make([]server.ServerPrompt, 0)
	for {
		listed, err := cl.ListPrompts(ctx, promptsRequest)
		if err != nil {
			return err
		}
		if listed == nil {
			return fmt.Errorf("<%s> ListPrompts returned nil response without error", c.name)
		}
		if len(listed.Prompts) == 0 {
			break
		}
		slog.Debug("Successfully listed prompts", "client", c.name, "count", len(listed.Prompts))
		for _, prompt := range listed.Prompts {
			slog.Debug("Adding prompt", "client", c.name, "prompt", prompt.Name)
			// Resolve the live client at call time so a reconnect is
			// transparent to the handler. The per-request bound is applied here
			// too: a stdio downstream shares one pipe for every request, so an
			// unanswered prompt wedges the whole route exactly as an unanswered
			// tool call would.
			prompts = append(prompts, server.ServerPrompt{Prompt: prompt, Handler: func(ctx context.Context, request mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				live := c.getClient()
				if live == nil {
					return nil, errors.New("downstream is not connected")
				}
				callCtx, cancel := c.withRequestTimeout(ctx)
				defer cancel()
				result, err := live.GetPrompt(callCtx, request)
				return result, config.RedactURLCredentials(err)
			}})
		}
		if listed.NextCursor == "" {
			break
		}
		promptsRequest.Params.Cursor = listed.NextCursor
	}
	mcpServer.SetPrompts(prompts...)
	return nil
}

func (c *Client) addResourcesToServer(ctx context.Context, mcpServer *server.MCPServer) error {
	cl := c.getClient()
	if cl == nil {
		return errors.New("downstream is not connected")
	}
	resourcesRequest := mcp.ListResourcesRequest{}
	// Collect and replace, so a resource the downstream dropped is not left
	// registered after a reconnect.
	resources := make([]server.ServerResource, 0)
	for {
		listed, err := cl.ListResources(ctx, resourcesRequest)
		if err != nil {
			return err
		}
		if listed == nil {
			return fmt.Errorf("<%s> ListResources returned nil response without error", c.name)
		}
		if len(listed.Resources) == 0 {
			break
		}
		slog.Debug("Successfully listed resources", "client", c.name, "count", len(listed.Resources))
		for _, resource := range listed.Resources {
			slog.Debug("Adding resource", "client", c.name, "resource", resource.Name)
			resources = append(resources, server.ServerResource{Resource: resource, Handler: func(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
				return c.readResource(ctx, request)
			}})
		}
		if listed.NextCursor == "" {
			break
		}
		resourcesRequest.Params.Cursor = listed.NextCursor

	}
	mcpServer.SetResources(resources...)
	return nil
}

func (c *Client) addResourceTemplatesToServer(ctx context.Context, mcpServer *server.MCPServer) error {
	cl := c.getClient()
	if cl == nil {
		return errors.New("downstream is not connected")
	}
	resourceTemplatesRequest := mcp.ListResourceTemplatesRequest{}
	// Collect and replace, so a template the downstream dropped is not left
	// registered after a reconnect.
	resourceTemplates := make([]server.ServerResourceTemplate, 0)
	for {
		listed, err := cl.ListResourceTemplates(ctx, resourceTemplatesRequest)
		if err != nil {
			return err
		}
		if listed == nil || len(listed.ResourceTemplates) == 0 {
			break
		}
		slog.Debug("Successfully listed resource templates", "client", c.name, "count", len(listed.ResourceTemplates))
		for _, resourceTemplate := range listed.ResourceTemplates {
			// A downstream that omits uriTemplate (or sends null) decodes to a
			// nil URITemplate, which mcp-go dereferences when it builds the
			// catalog key (entry.Template.URITemplate.Raw()). Registering it
			// panics the whole proxy - the shared process serves every route -
			// so a template without a usable URI template is skipped instead.
			if resourceTemplate.URITemplate == nil || resourceTemplate.URITemplate.Raw() == "" {
				slog.Warn("Skipping resource template without a uriTemplate", "client", c.name, "template", resourceTemplate.Name)
				continue
			}
			slog.Debug("Adding resource template", "client", c.name, "template", resourceTemplate.Name)
			resourceTemplates = append(resourceTemplates, server.ServerResourceTemplate{Template: resourceTemplate, Handler: func(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
				return c.readResource(ctx, request)
			}})
		}
		if listed.NextCursor == "" {
			break
		}
		resourceTemplatesRequest.Params.Cursor = listed.NextCursor
	}
	mcpServer.SetResourceTemplates(resourceTemplates...)
	return nil
}

// readResource resolves the live client at call time so a reconnect is
// transparent to the handler. Like callTool it applies the configured
// per-request bound: an unanswered resource read occupies the single stdio
// channel and takes the whole downstream unhealthy for every caller.
func (c *Client) readResource(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	cl := c.getClient()
	if cl == nil {
		return nil, errors.New("downstream is not connected")
	}
	callCtx, cancel := c.withRequestTimeout(ctx)
	defer cancel()
	readResource, err := cl.ReadResource(callCtx, request)
	if err != nil {
		return nil, config.RedactURLCredentials(err)
	}
	return readResource.Contents, nil
}

// lazyLoadPending reports whether this downstream is lazy-loaded and has not
// been activated yet.
func (c *Client) lazyLoadPending() bool {
	return c.options != nil && c.options.LazyLoad.OrElse(false) && !c.activated.Load()
}

// lazySummary is what the activation meta-tool tells the caller about the
// catalog it would mount.
type lazySummary struct {
	tools                                 []mcp.Tool
	prompts, resources, resourceTemplates int
}

// summarize lists the downstream's catalog without mounting it.
func (c *Client) summarize(ctx context.Context) (lazySummary, error) {
	var sum lazySummary
	cl := c.getClient()
	if cl == nil {
		return sum, errors.New("downstream is not connected")
	}
	keep := toolFilterFunc(c.name, c.options)
	toolsRequest := mcp.ListToolsRequest{}
	for {
		listed, err := cl.ListTools(ctx, toolsRequest)
		if err != nil {
			return sum, err
		}
		if listed == nil {
			break
		}
		for _, tool := range listed.Tools {
			if keep(tool.Name) {
				sum.tools = append(sum.tools, tool)
			}
		}
		if listed.NextCursor == "" || len(listed.Tools) == 0 {
			break
		}
		toolsRequest.Params.Cursor = listed.NextCursor
	}
	// Prompts and resources are optional capabilities; a downstream without
	// them just reports zero.
	promptsRequest := mcp.ListPromptsRequest{}
	for {
		listed, err := cl.ListPrompts(ctx, promptsRequest)
		if err != nil || listed == nil {
			break
		}
		sum.prompts += len(listed.Prompts)
		if listed.NextCursor == "" || len(listed.Prompts) == 0 {
			break
		}
		promptsRequest.Params.Cursor = listed.NextCursor
	}
	resourcesRequest := mcp.ListResourcesRequest{}
	for {
		listed, err := cl.ListResources(ctx, resourcesRequest)
		if err != nil || listed == nil {
			break
		}
		sum.resources += len(listed.Resources)
		if listed.NextCursor == "" || len(listed.Resources) == 0 {
			break
		}
		resourcesRequest.Params.Cursor = listed.NextCursor
	}
	templatesRequest := mcp.ListResourceTemplatesRequest{}
	for {
		listed, err := cl.ListResourceTemplates(ctx, templatesRequest)
		if err != nil || listed == nil {
			break
		}
		sum.resourceTemplates += len(listed.ResourceTemplates)
		if listed.NextCursor == "" || len(listed.ResourceTemplates) == 0 {
			break
		}
		templatesRequest.Params.Cursor = listed.NextCursor
	}
	return sum, nil
}

// activateTools mounts the real tools, prompts and resources of a lazy-loaded
// downstream. It is the handler of the activate_<server> meta-tool.
//
// Mounting the catalog replaces the route's tool set, so the meta-tool is
// mounted again afterwards: callers that activate defensively keep working.
func (c *Client) activateTools(mcpServer *server.MCPServer, meta mcp.Tool) server.ToolHandlerFunc {
	var handler server.ToolHandlerFunc
	handler = func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		c.activateMu.Lock()
		defer c.activateMu.Unlock()

		sum, err := c.summarize(ctx)
		if err != nil {
			return nil, err
		}
		if !c.activated.Load() {
			slog.Info("Activating lazy-loaded downstream", "client", c.name, "tools", len(sum.tools), "prompts", sum.prompts, "resources", sum.resources, "templates", sum.resourceTemplates)
			if err := c.registerCatalogSafely(func() error {
				return c.addToolsToServer(ctx, mcpServer)
			}); err != nil {
				return nil, err
			}
			_ = c.registerCatalogSafely(func() error {
				_ = c.addPromptsToServer(ctx, mcpServer)
				_ = c.addResourcesToServer(ctx, mcpServer)
				_ = c.addResourceTemplatesToServer(ctx, mcpServer)
				return nil
			})
			mcpServer.AddTool(meta, handler)
			c.activated.Store(true)
		}

		body, err := json.Marshal(map[string]any{
			"activated":     true,
			"server":        c.name,
			"toolCount":     len(sum.tools),
			"promptCount":   sum.prompts,
			"resourceCount": sum.resources,
			"templateCount": sum.resourceTemplates,
		})
		if err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.NewTextContent(string(body))}}, nil
	}
	return handler
}

// registerMetaTool mounts the single activate_<server> tool in place of the
// downstream's catalog.
func (c *Client) registerMetaTool(ctx context.Context, mcpServer *server.MCPServer) error {
	sum, err := c.summarize(ctx)
	if err != nil {
		return err
	}
	metaToolName := fmt.Sprintf("activate_%s", c.name)

	var description string
	switch c.name {
	case "serena":
		description = "Activate Serena MCP server. Provides semantic code operations, symbol finding, file editing, and code analysis tools. "
	case "playwright":
		description = "Activate Playwright MCP server. Provides browser automation, web scraping, screenshots, and web interaction tools. "
	default:
		description = fmt.Sprintf("Activate and load all tools from the %s MCP server. ", c.name)
	}
	description += fmt.Sprintf("This will load %d tools", len(sum.tools))
	if sum.prompts > 0 {
		description += fmt.Sprintf(", %d prompts", sum.prompts)
	}
	if sum.resources > 0 {
		description += fmt.Sprintf(", %d resources", sum.resources)
	}
	if sum.resourceTemplates > 0 {
		description += fmt.Sprintf(", %d resource templates", sum.resourceTemplates)
	}
	description += "."
	if len(sum.tools) > 0 {
		const preview = 5
		names := make([]string, 0, preview)
		for _, tool := range sum.tools[:min(preview, len(sum.tools))] {
			names = append(names, tool.Name)
		}
		description += " Available tools include: " + strings.Join(names, ", ")
		if len(sum.tools) > preview {
			description += fmt.Sprintf(" and %d more", len(sum.tools)-preview)
		}
		description += "."
	}

	slog.Info("Registering activation meta-tool", "client", c.name, "tool", metaToolName)
	meta := mcp.Tool{
		Name:        metaToolName,
		Description: description,
		InputSchema: mcp.ToolInputSchema{Type: "object", Properties: map[string]any{}},
	}
	mcpServer.SetTools(server.ServerTool{Tool: meta, Handler: c.activateTools(mcpServer, meta)})
	return nil
}

// Connect brings the downstream up without mounting its catalog on a proxy
// server. The hierarchy router uses it: it lists and calls tools itself.
func (c *Client) Connect(ctx context.Context, clientInfo mcp.Implementation) error {
	c.clientInfo = clientInfo
	return c.connect(ctx, clientInfo, nil)
}

// NeedPing reports whether the client runs a keepalive probe.
func (c *Client) NeedPing() bool {
	return c.needPing
}

// StartPing starts the keepalive (and, when configured, reconnect) loop. It is
// idempotent, and stops when ctx is done.
func (c *Client) StartPing(ctx context.Context) {
	if !c.needPing {
		return
	}
	c.pingOnce.Do(func() {
		go c.startPingTask(ctx)
	})
}

// GetClient returns the live underlying mcp-go client, or nil before the first
// connection.
func (c *Client) GetClient() *client.Client {
	return c.getClient()
}

// NewInProcess wraps an already-connected mcp-go client. It exists so tests
// elsewhere in this module can exercise code that takes a *Client against an
// in-process MCP server, without spawning a subprocess or opening a socket.
func NewInProcess(name string, raw *client.Client) *Client {
	return &Client{name: name, client: raw}
}

// Kill force-terminates the stdio subprocess behind the client, if there is
// one. Close cannot be trusted to return for a wedged process.
func (c *Client) Kill() error {
	c.mu.RLock()
	cmd := c.cmd
	c.mu.RUnlock()
	if cmd != nil && cmd.Process != nil {
		return cmd.Process.Kill()
	}
	return nil
}

// CloseWithTimeout closes the client and force-kills the subprocess if Close
// has not returned within d. Plain Close only closes stdin and then blocks on
// cmd.Wait(), which never returns for a process that ignores stdin EOF. Killing
// the process also unblocks that wait, so the goroutine is not leaked.
func (c *Client) CloseWithTimeout(d time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- c.Close() }()

	select {
	case err := <-done:
		return err
	case <-time.After(d):
		_ = c.Kill()
		return fmt.Errorf("<%s> close timed out after %s, force-killed process", c.name, d)
	}
}

func (c *Client) Close() error {
	c.closed.Store(true)
	cl := c.getClient()
	if cl == nil {
		return nil
	}
	err := cl.Close()
	// A stdio subprocess that already died, or that exits non-zero when its
	// stdin is closed (many MCP servers do), is not a failure to close it.
	// Reporting it as one would make the proxy exit non-zero after an
	// otherwise clean shutdown.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		slog.Warn("Downstream server exited with an error", "client", c.name, "err", err)
		return nil
	}
	return err
}

type Server struct {
	mcpServer *server.MCPServer
	handler   http.Handler
}

func NewMCPServer(name string, serverConfig *config.MCPProxyConfigV2, clientConfig *config.MCPClientConfigV2) (*Server, error) {
	if serverConfig == nil {
		return nil, errors.New("server config is required")
	}
	if clientConfig == nil {
		return nil, errors.New("client config is required")
	}
	clientOptions := clientConfig.Options
	if clientOptions == nil {
		clientOptions = &config.OptionsV2{}
	}
	serverOpts := []server.ServerOption{
		server.WithResourceCapabilities(true, true),
		server.WithRecovery(),
	}

	if clientOptions.LogEnabled.OrElse(false) {
		serverOpts = append(serverOpts, server.WithLogging())
	}
	mcpServer := server.NewMCPServer(
		name,
		serverConfig.Version,
		serverOpts...,
	)

	var handler http.Handler

	switch serverConfig.Type {
	case config.MCPServerTypeSSE:
		handler = server.NewSSEServer(
			mcpServer,
			server.WithStaticBasePath(name),
			server.WithBaseURL(serverConfig.BaseURL),
		)
	case config.MCPServerTypeStreamable:
		handler = server.NewStreamableHTTPServer(
			mcpServer,
			server.WithStateLess(true),
		)
	default:
		return nil, fmt.Errorf("unknown server type: %s", serverConfig.Type)
	}
	return &Server{
		mcpServer: mcpServer,
		handler:   handler,
	}, nil
}
