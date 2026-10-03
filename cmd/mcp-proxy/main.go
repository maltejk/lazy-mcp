package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/voicetreelab/lazy-mcp/internal/client"
	"github.com/voicetreelab/lazy-mcp/internal/config"
	"github.com/voicetreelab/lazy-mcp/internal/server"
)

var BuildVersion = "dev"

func main() {
	confPath := flag.String("config", "config.json", "path to config file or a http(s) url")
	insecure := flag.Bool("insecure", false, "allow insecure HTTPS connections by skipping TLS certificate verification")
	expandEnv := flag.Bool("expand-env", true, "expand environment variables in config file")
	port := flag.String("port", "", "port to listen on (overrides config), e.g. '8080' or ':8080'")
	hierarchy := flag.String("hierarchy", "", "path to hierarchy directory (overrides config); serves the get_tools_in_category/execute_tool meta-tools instead of mounting every downstream tool")
	httpHeaders := flag.String("http-headers", "", "optional HTTP headers for config URL, format: 'Key1:Value1;Key2:Value2'")
	httpTimeout := flag.Int("http-timeout", 10, "HTTP timeout in seconds when fetching config from URL")
	authorize := flag.String("authorize", "", "run a one-time interactive OAuth authorization for the named mcpServers entry, then exit. Opens a browser; run this by hand, not from the daemon/service.")
	checkConfig := flag.Bool("check-config", false, "load and validate the config, then exit without starting the server")
	authStatus := flag.Bool("auth-status", false, "list every configured MCP server with its transport and authentication state, then exit. Local-only: reads config.json and cached OAuth token expiry, makes no network calls.")
	doctor := flag.Bool("doctor", false, "like -auth-status, but also connects to each remote server to confirm its credentials are accepted right now (may refresh an expired OAuth token via its refresh token; never opens a browser)")
	var logLevel slog.Level
	flag.TextVar(&logLevel, "log-level", slog.LevelInfo, "log level (debug, info, warn, error)")

	version := flag.Bool("version", false, "print version and exit")
	help := flag.Bool("help", false, "print help and exit")
	flag.Parse()
	if *help {
		flag.Usage()
		return
	}
	if *version {
		fmt.Println(BuildVersion)
		return
	}
	client.BuildVersion = BuildVersion
	setLogOutput(os.Stderr, logLevel)
	if *authorize != "" {
		if err := client.RunAuthorize(*confPath, *authorize, *insecure, *expandEnv, *httpHeaders, *httpTimeout); err != nil {
			slog.Error("Failed to authorize server", "server", *authorize, "err", config.RedactURLCredentials(err))
			os.Exit(1)
		}
		return
	}
	if *authStatus || *doctor {
		ok, err := client.RunDoctor(*confPath, *insecure, *expandEnv, *httpHeaders, *httpTimeout, *doctor)
		if err != nil {
			slog.Error("Failed to run doctor", "err", config.RedactURLCredentials(err))
			os.Exit(1)
		}
		if !ok {
			os.Exit(1)
		}
		return
	}
	conf, err := config.Load(*confPath, *insecure, *expandEnv, *httpHeaders, *httpTimeout)
	if err != nil {
		slog.Error("Failed to load config", "err", config.RedactURLCredentials(err))
		os.Exit(1)
	}
	if *port != "" {
		if (*port)[0] != ':' {
			conf.McpProxy.Addr = ":" + *port
		} else {
			conf.McpProxy.Addr = *port
		}
	}
	if *hierarchy != "" {
		conf.McpProxy.HierarchyPath = *hierarchy
	}
	if *checkConfig {
		fmt.Printf("Config OK: %d MCP server(s) configured\n", len(conf.McpServers))
		return
	}

	// Stdio mode owns stdout for the MCP protocol, so a log file is the only
	// place logs can go besides stderr. It is honoured in every mode.
	if opts := conf.McpProxy.Options; opts != nil && opts.LogFilePath != "" {
		logFile, openErr := os.OpenFile(opts.LogFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if openErr != nil {
			slog.Error("Failed to open log file", "path", opts.LogFilePath, "err", openErr)
			os.Exit(1)
		}
		defer logFile.Close()
		setLogOutput(io.MultiWriter(os.Stderr, logFile), logLevel)
	}

	switch {
	case conf.McpProxy.Type == config.MCPServerTypeStdio:
		err = startHierarchy(conf, server.StartStdioServer)
	case conf.McpProxy.HierarchyPath != "":
		err = startHierarchy(conf, server.StartHTTPServer)
	default:
		err = client.StartHTTPServer(conf)
	}
	if err != nil {
		slog.Error("Failed to start server", "err", config.RedactURLCredentials(err))
		os.Exit(1)
	}
}

func setLogOutput(w io.Writer, level slog.Level) {
	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})))
}

// startHierarchy runs one of the meta-tool servers, which need a hierarchy
// directory to expose anything.
func startHierarchy(conf *config.Config, start func(*config.Config) error) error {
	if conf.McpProxy.HierarchyPath == "" {
		return fmt.Errorf("a hierarchy directory is required: set mcpProxy.hierarchyPath or pass -hierarchy")
	}
	return start(conf)
}
