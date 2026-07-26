package protocol_test

import (
	"testing"

	"github.com/fingerskier/langer/protocol"
)

func TestWorkspaceIDForRootIsDeterministicAndRootScoped(t *testing.T) {
	t.Parallel()
	first := protocol.WorkspaceIDForRoot("/canonical/repo")
	if first == "" {
		t.Fatal("WorkspaceIDForRoot returned an empty id")
	}
	if got := protocol.WorkspaceIDForRoot("/canonical/repo"); got != first {
		t.Fatalf("same root produced %q then %q", first, got)
	}
	if other := protocol.WorkspaceIDForRoot("/other/clone"); other == first {
		t.Fatalf("different roots shared id %q", first)
	}
}
