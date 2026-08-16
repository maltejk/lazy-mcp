// Package e2e holds the end-to-end tests. They drive the compiled mcp-proxy
// binary as a real process rather than calling into the package, so they live
// outside the root package and can run in parallel.
//
// The files are:
//
//   - helpers_test.go  building, booting and talking to the proxy
//   - stdio_test.go    proxying a stdio subprocess, over both proxy transports
//   - remote_test.go   proxying a remote sse/streamable-http server
//   - cli_test.go      the command line: -version, -check-config, -auth-status
//
// Everything here spawns processes and binds sockets, so every test skips
// under -short. Unit tests live next to the code they cover, in the root
// package.
package e2e
