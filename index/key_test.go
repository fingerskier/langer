package index

import (
	"testing"

	"github.com/fingerskier/langer/protocol"
)

func TestStableKeyAndSymbolKey(t *testing.T) {
	t.Parallel()

	symbol := protocol.Symbol{
		Name:      "Serve",
		Kind:      protocol.SymbolKindMethod,
		Container: "Server",
	}
	stable := StableKey(symbol)
	if want := "Serve\x1fServer\x1fmethod"; stable != want {
		t.Fatalf("StableKey() = %q, want %q", stable, want)
	}

	got := SymbolKey("fingerskier/langer", `internal\server.go`, stable)
	want := "fingerskier/langer\x1finternal/server.go\x1fServe\x1fServer\x1fmethod"
	if got != want {
		t.Fatalf("SymbolKey() = %q, want %q", got, want)
	}
}

func TestSymbolKeyPreservesCanonicalRootFallbackNamespaceExactly(t *testing.T) {
	t.Parallel()

	stable := "Serve\x1fServer\x1fmethod"
	for _, namespace := range []string{
		"/tmp/worktrees/langer",
		`C:\dev\worktrees\langer`,
	} {
		got := SymbolKey(namespace, "internal/server.go", stable)
		want := namespace + "\x1finternal/server.go\x1f" + stable
		if got != want {
			t.Errorf("SymbolKey(%q) = %q, want %q", namespace, got, want)
		}
	}
}
