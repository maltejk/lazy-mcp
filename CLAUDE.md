# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`lazy-mcp` (module `github.com/voicetreelab/lazy-mcp`) is an MCP proxy/router. Instead of exposing every
downstream MCP server's tools directly, it exposes exactly **two meta-tools** to the calling agent:

- `get_tools_in_category(path)` — navigate a hierarchical tree of tool categories
- `execute_tool(tool_path, arguments)` — invoke a tool by its dotted path, proxied to the real MCP server

This keeps most tool schemas out of the agent's context until it actually needs them (hence "lazy"). Forked
from `TBXark/mcp-proxy` and extended with hierarchical routing, lazy loading, and stdio support. Without a
hierarchy the binary still runs as plain upstream mcp-proxy ("proxy mode", see below), so upstream features
(OAuth, health endpoints, auto-reconnect, `-doctor`) keep working.

## Commands

```bash
make build            # builds ./build/mcp-proxy and ./build/structure_generator
make buildLinuxX86     # cross-compile for linux/amd64
make format            # go fix/fmt/vet, tests, go mod tidy, golangci-lint fmt+run, nilaway
make buildImage         # multi-arch docker build+push (ghcr.io/tbxark/map-proxy)

go test ./...                          # run all tests (includes the process-spawning e2e tests; see the root-package note below)
go test -short ./...                   # unit tests only
go test ./internal/hierarchy/...       # single package
go test -run TestGetClientMutex ./internal/hierarchy/...   # single test
cd structure_generator && go test -v   # structure_generator has its own test suite

./build/mcp-proxy --config config.json           # run the router (stdio or http, per config)
./build/mcp-proxy --config config.json --port 8080 --hierarchy testdata/mcp_hierarchy
./build/mcp-proxy --config config.json -check-config   # validate config and exit
./build/structure_generator --config config.json --output testdata/mcp_hierarchy   # generate hierarchy JSON
```

Notes:

- `structure_generator/generator_test.go.skip` is intentionally disabled (`.skip` suffix); don't assume it
  runs under `go test`.
- The root package's tests (`recursive_lazy_load_test.go`: `TestRecursiveLazyLoadingFlow`,
  `TestHierarchyConfigLoading`, `TestToolPathParsing`, `TestErrorHandling`, `TestDocumentationGeneration`)
  fail, even with `-short`: they expect a `coding_tools/serena` hierarchy that `testdata/mcp_hierarchy`
  (which holds the `everything` server) does not contain. That predates the move onto upstream, so don't
  mistake those failures for a regression; every other package passes.
- The Go toolchain may not be on `PATH`; here it lives in `~/.local/go/bin`. `go.mod` needs Go 1.25.5,
  which the `go` command downloads on first use.

## Architecture

### Request flow (`cmd/mcp-proxy`)

`cmd/mcp-proxy/main.go` parses flags, loads config via `internal/config`, then picks a mode:

| condition | mode |
|---|---|
| `mcpProxy.type: stdio` | `server.StartStdioServer` — meta-tools over stdin/stdout |
| `mcpProxy.hierarchyPath` set (or `-hierarchy`) | `server.StartHTTPServer` — meta-tools over SSE/streamable HTTP |
| otherwise | `client.StartHTTPServer` — **proxy mode**: upstream mcp-proxy, one route per downstream server |
| `-authorize`, `-auth-status`/`-doctor` | upstream OAuth/diagnostic commands, then exit |

The two meta-tool servers share `newMetaToolServer` in `internal/server`, so meta-tool behavior is changed in
one place.

### `internal/hierarchy` — the core lazy-routing logic

- **`Hierarchy`** loads a tree of `HierarchyNode`s from a directory of JSON files (`LoadHierarchy`). Each
  file is either a branch (has `categories`/an overview, no direct server) or a leaf (has `tools` and
  optionally an `mcp_server` block). The hierarchy key for a node is derived from its file path: a
  nested `dir/dir.json` maps to key `dir`, while a flat `dir/tool.json` maps to key `dir.tool`.
- **`HandleGetToolsInCategory(path)`** walks `h.nodes` to find direct children of `path` and returns
  their overviews / aggregated tool lists — this is what backs `get_tools_in_category`.
- **`ResolveToolPath(toolPath)`** resolves a dotted tool path to a `ToolDefinition`, trying the full path
  first, then progressively shorter parent categories (so tools can be referenced by short or full path).
- **`ServerRegistry`** lazily creates and connects the `internal/client.Client` for a downstream MCP server
  on first use (`GetOrLoadServer` → `loadServer` → `Client.Connect`), and hands out a **per-server mutex**
  (`GetClientMutex`) that `HandleExecuteTool` holds around every tool call — stdio is a single-channel
  transport, so concurrent calls to the same stdio server must be serialized (see the mutex comment and
  issue #8 referenced in code). A server that errors at the transport level is evicted
  (`EvictServer`: bounded close, force-kill) and respawned on a later call after `restartCooldown`. The
  registry owns a long-lived context (`r.ctx`): downstream sessions and health probes run under it, never
  under a per-request context.
- `coerce.go` coerces LLM-supplied argument types to the declared schema types; errors carry the tool's
  `inputSchema` so the caller can self-correct.

### `internal/client`

Everything that talks to a downstream MCP server, plus the upstream proxy-mode server and OAuth/doctor
commands. These share one package (rather than separate `oauth`/`server` packages) because upstream's code
is tightly coupled — the client uses OAuth, OAuth uses the client — and its white-box tests depend on that.

- `client.go` — `Client` wraps `mark3labs/mcp-go` for stdio/SSE/streamable-http. `connect` builds the transport,
  swaps it in under `mu`, then (proxy mode) mounts the downstream's tools/prompts/resources. A keepalive
  probe loop (`options.pingInterval`) drives health; with `autoReconnect` it also rebuilds a dropped
  connection. `Start` gets the long-lived context and only `Initialize` is bounded (`connectTimeout`).
  `Connect` is the same handshake without mounting a catalog — what the hierarchy registry uses.
- Fork additions in `client.go`: `options.lazyLoad` mounts one `activate_<server>` meta-tool instead of the
  catalog until it is called (a different mechanism from the hierarchy router; don't conflate the two);
  `Kill`/`CloseWithTimeout` force-terminate a stdio subprocess (mcp-go's `Close` blocks on `cmd.Wait()`);
  `stderr_watch.go` keeps the last stderr lines and lets a crash during startup abort the handshake early
  with the server's own diagnostics.
- `http.go` — proxy-mode HTTP server: per-server routes, health endpoints, startup/reconnect goroutines.
- `oauth.go`, `oauth_store.go`, `doctor.go` — OAuth client support (`-authorize`), token persistence, and the
  `-auth-status`/`-doctor` diagnostics.
- `main.BuildVersion` is injected via ldflags and copied to `client.BuildVersion` at startup.

### `internal/config`

Config loading via `go-sphere/confstore`, supporting local files or remote HTTP(S) URLs, with
`${VAR_NAME}` environment-variable expansion. Only the V2 schema (`mcpProxy`/`mcpServers`) exists: V1 was
removed upstream, and `FullConfig` carries the old `server`/`clients` keys purely so a V1 file fails with a
migration hint. Per-server options fall back to the `mcpProxy.options` value when unset on the individual
server. `redact.go` holds URL-credential redaction and redirect-URI parsing, which both config validation and
the client need. Fork additions: `mcpProxy.type: stdio`, `hierarchyPath`, and options `logFilePath`,
`debugLogging`, `lazyLoad`, `recursiveLazyLoad`.

### Key behaviors to preserve (from upstream)

- HTTP clients force `Accept-Encoding: identity` (`mcpHTTPHeaders`): some MCP servers otherwise send gzip that
  mcp-go's JSON decoder can't handle.
- `/_healthz` and `/_readyz` are unauthenticated (proxy mode). `/_readyz` is 503 until every client mounts and
  503 `degraded` once a client that had connected fails `pingFailureThreshold` probes in a row.
- A failed downstream connection is logged but non-fatal unless that server sets `panicIfInvalid`.
- `autoReconnect` is opt-in; `panicIfInvalid` is checked first and still fails fast.
- `Client.Close` swallows `*exec.ExitError`: a stdio server that exits non-zero when stdin closes is not a
  shutdown failure.
- OAuth dynamic client registration (empty `clientId`) is reloaded from disk on daemon start.

### Hierarchy JSON format

See `docs/CONFIGURATION.md` for the full spec. Key points:
- A node file has `overview`, `categories` (subcategory name → description), and `tools` (name → tool def).
- `mcp_server` (on a leaf/category node) declares the downstream server's transport (`stdio`/`sse`/
  `streamable-http`) and is inherited by child categories.
- A tool's `maps_to` renames the hierarchy tool name to the real downstream tool name; `server` names which
  configured MCP server owns it.
- `structure/` and `testdata/mcp_hierarchy/` hold example/generated hierarchies; `examples/synergy/` holds
  a multi-role example (arc/dev/po/qa/tl configs each with their own generated `-structure` dir), used to
  run one lazy-mcp instance per agent role.

### `structure_generator`

A separate CLI/library (`structure_generator/generator.go`, `types.go`, `cmd/main.go`) that generates the
hierarchy JSON tree described above, either from pre-fetched tool JSON (`-input`, recommended) or by
querying live MCP servers (`-config`, experimental — stdio server connections can hang during
initialization; prefer fetching tools via a running `mcp-proxy`'s HTTP endpoint instead, see
`structure_generator/README.md`). Supports `-regenerate-root` to rebuild parent overview/category JSON
after manually moving tool folders around in the output tree (manual overview edits are preserved).

### Permission control for proxied tools

Because all downstream calls go through the single `execute_tool` meta-tool, per-tool MCP permission
rules (e.g. `mcp__github__create_issue`) don't apply. `examples/hooks/` and `examples/plugins/` implement
permission gating by inspecting the `tool_path`/`arguments` of `execute_tool` calls instead (Claude Code
`PreToolUse` hook, a token-confirmation hook for agents without the permission-decision protocol, and an
OpenCode plugin). See the README's "Permission Control with Claude Code Hooks" section for the
`LAZY_MCP_SENSITIVE_TOOLS`/`LAZY_MCP_DENIED_TOOLS` pattern syntax.

## Tests

```
internal/<pkg>/*_test.go   unit tests next to the code they cover
test/e2e/                  end-to-end tests (package e2e), skipped under -short
testdata/stdio-server      downstream MCP server fixture
testdata/mcp_hierarchy     example hierarchy
```

The e2e tests drive the **compiled binary** (`./cmd/mcp-proxy`) as a subprocess, so they cannot patch package
variables: the timings they depend on are config fields (`mcpProxy.startupGracePeriod`, `options.pingInterval`).
`test/e2e/helpers_test.go` builds the binaries once and boots the proxy; `stdio_test.go`, `remote_test.go`,
`cli_test.go` and the `stdio_*` timeout/stderr tests cover proxy mode end to end.

## Docs

`docs/CONFIGURATION.md`, `docs/USAGE.md`, `docs/DEPLOYMENT.md` document the config schema, CLI flags/endpoints, and
deployment. Update them when changing flags or config fields. `docs/index.html` is the online Claude-config
converter published via GitHub Pages.
