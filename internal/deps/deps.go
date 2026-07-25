// Package deps pins the third-party dependencies later milestones need, so
// `go mod tidy` cannot drop them before the code that uses them exists.
//
// M0 owns go.mod; M1–M6 should never have to touch it. Each import below is
// claimed by a specific milestone:
//
//	modernc.org/sqlite                       M3 — pure-Go SQLite driver (no cgo,
//	                                         keeps the single-binary goal)
//	github.com/fsnotify/fsnotify             M3 — workspace file watcher
//	github.com/modelcontextprotocol/go-sdk   M4 — MCP server over stdio
//
// The TOML decoder and go-cmp are already used directly (config, tests) and so
// need no entry here.
//
// This package is imported by nothing. Delete an import once the owning
// milestone imports it for real, and delete the package when the last one goes.
package deps

import (
	_ "github.com/fsnotify/fsnotify"
	_ "github.com/modelcontextprotocol/go-sdk/mcp"
	_ "modernc.org/sqlite"
)
