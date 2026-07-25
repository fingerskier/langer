package wire

import (
	"net/url"
	"path/filepath"
	"strings"

	"github.com/fingerskier/langer/protocol"
)

// ToPosition converts an LSP position to a protocol one. Both are 0-based with
// UTF-16 characters (SPEC §4.3), so this is a rename, not a conversion — which
// is exactly why the encoding must never be negotiated away.
func ToPosition(p RawPosition) protocol.Position {
	return protocol.Position{Line: p.Line, Character: p.Character}
}

// ToRange converts an LSP range to a protocol one.
func ToRange(r RawRange) protocol.Range {
	return protocol.Range{Start: ToPosition(r.Start), End: ToPosition(r.End)}
}

// FromPosition converts a protocol position back to the LSP wire shape.
func FromPosition(p protocol.Position) RawPosition {
	return RawPosition{Line: p.Line, Character: p.Character}
}

// ToLocations normalises raw locations into SPEC §4.4 Location values.
//
// lines supplies the previews and must be the LineIndex of the file the raws
// belong to; callers group by URI first. Results outside root are DROPPED: a
// live query may legitimately resolve into a dependency (SPEC §3.4), and the
// bridge only speaks workspace-relative paths.
//
// IsDefinition is left false here. Only the caller knows whether it asked for
// definitions or for references, and marking it in a pure function would
// require guessing.
func ToLocations(root string, raws []RawLocation, lines LineIndex) ([]protocol.Location, error) {
	out := make([]protocol.Location, 0, len(raws))
	for _, r := range raws {
		rel, ok := URIToPath(root, r.URI)
		if !ok {
			continue
		}
		out = append(out, protocol.Location{
			Path:    rel,
			Range:   ToRange(r.Range),
			Preview: preview(lines.Line(r.Range.Start.Line)),
		})
	}
	return out, nil
}

// preview renders a source line for a Location. It is trimmed because leading
// indentation is pure token cost to an agent.
func preview(line string) string {
	return strings.TrimSpace(line)
}

// FlattenSymbols turns a possibly hierarchical symbol tree into the SPEC §4.4
// flat list, deriving `container` from the parent chain (docs §10.4 — Symbol
// deliberately has no children field).
//
// relPath is used for DocumentSymbol results, which carry no URI of their own.
// SymbolInformation results DO carry one — that is what workspace/symbol
// returns — and their own URI always wins.
func FlattenSymbols(root, relPath string, raws []RawSymbol) []protocol.Symbol {
	var out []protocol.Symbol
	flattenInto(&out, root, relPath, "", raws)
	return out
}

func flattenInto(out *[]protocol.Symbol, root, relPath, container string, raws []RawSymbol) {
	for _, r := range raws {
		path := relPath
		if r.URI != "" {
			rel, ok := URIToPath(root, r.URI)
			if !ok {
				continue // a symbol in a dependency: never ours to report
			}
			path = rel
		}

		// An explicit containerName (SymbolInformation) wins over the derived
		// parent chain; pyright populates it, tsserver does not.
		effective := container
		if r.Container != "" {
			effective = r.Container
		}

		*out = append(*out, protocol.Symbol{
			Name:      r.Name,
			Kind:      protocol.SymbolKindFromLSP(r.Kind),
			Container: effective,
			Path:      path,
			Range:     ToRange(r.Range),
			Detail:    r.Detail,
		})

		if len(r.Children) > 0 {
			flattenInto(out, root, path, r.Name, r.Children)
		}
	}
}

// ToDiagnostics normalises raw diagnostics into SPEC §4.4 Diagnostic values.
// Code and Source are carried VERBATIM (docs §10.5).
func ToDiagnostics(root string, raws []RawDiagnostic) ([]protocol.Diagnostic, error) {
	out := make([]protocol.Diagnostic, 0, len(raws))
	for _, r := range raws {
		rel, ok := URIToPath(root, r.URI)
		if !ok {
			continue
		}
		out = append(out, protocol.Diagnostic{
			Path:     rel,
			Severity: protocol.SeverityFromLSP(r.Severity),
			Code:     r.Code,
			Source:   r.Source,
			Message:  r.Message,
			Range:    ToRange(r.Range),
		})
	}
	return out, nil
}

// ToFileEdits normalises a decoded WorkspaceEdit into protocol.FileEdit values.
func ToFileEdits(root string, raws []RawFileEdit) ([]protocol.FileEdit, error) {
	out := make([]protocol.FileEdit, 0, len(raws))
	for _, r := range raws {
		rel, ok := URIToPath(root, r.URI)
		if !ok {
			continue
		}
		edits := make([]protocol.TextEdit, 0, len(r.Edits))
		for _, e := range r.Edits {
			edits = append(edits, protocol.TextEdit{Range: ToRange(e.Range), NewText: e.NewText})
		}
		out = append(out, protocol.FileEdit{Path: rel, Edits: edits})
	}
	return out, nil
}

// SplitHover separates a server's single markup blob into the SPEC §4.4
// contents/documentation pair:
//
//   - strip a leading newline (typescript-language-server emits one);
//   - the FIRST fenced code block, fences removed and trimmed, is contents;
//   - everything after it, with a leading "---" rule and blank lines removed,
//     is documentation (empty ⇒ field omitted);
//   - with no fenced block, the whole blob is contents.
//
// It is a heuristic, golden-tested against the payloads captured in
// testdata/README.md §1.6 and §2.6.
func SplitHover(markup string, rng *protocol.Range) *protocol.Hover {
	trimmed := strings.TrimSpace(markup)
	if trimmed == "" {
		return nil
	}

	contents, documentation := splitFencedBlock(trimmed)
	if contents == "" {
		return nil
	}
	return &protocol.Hover{Contents: contents, Documentation: documentation, Range: rng}
}

func splitFencedBlock(markup string) (contents, documentation string) {
	lines := strings.Split(markup, "\n")

	open := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			open = i
			break
		}
	}
	if open < 0 {
		return strings.TrimSpace(markup), ""
	}

	closing := -1
	for i := open + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "```" {
			closing = i
			break
		}
	}
	if closing < 0 {
		// An unterminated fence: treat everything after it as the signature
		// rather than losing the payload.
		return strings.TrimSpace(strings.Join(lines[open+1:], "\n")), ""
	}

	contents = strings.TrimSpace(strings.Join(lines[open+1:closing], "\n"))
	documentation = cleanDocumentation(lines[closing+1:])
	return contents, documentation
}

// cleanDocumentation drops the horizontal rule pyright puts between a signature
// and its docstring, plus surrounding blank lines.
func cleanDocumentation(lines []string) string {
	for len(lines) > 0 {
		first := strings.TrimSpace(lines[0])
		if first == "" || first == "---" || first == "___" || first == "***" {
			lines = lines[1:]
			continue
		}
		break
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// URIToPath converts a file:// URI to a workspace-relative slash path.
//
// ok is false when the URI is outside root — a dependency file the language
// server resolved into. SPEC §3.4 says those are never indexed, but they MAY
// legitimately appear in a live query result, in which case the caller drops
// them.
func URIToPath(root, uri string) (string, bool) {
	abs, ok := uriToAbs(uri)
	if !ok {
		return "", false
	}

	if rel, ok := relativeTo(root, abs); ok {
		return rel, true
	}
	// macOS reports /private/var where a caller passed /var (and vice versa);
	// a language server may use either spelling.
	if resolvedRoot, err := filepath.EvalSymlinks(root); err == nil && resolvedRoot != root {
		if rel, ok := relativeTo(resolvedRoot, abs); ok {
			return rel, true
		}
	}
	if resolvedAbs, err := filepath.EvalSymlinks(abs); err == nil && resolvedAbs != abs {
		if rel, ok := relativeTo(root, resolvedAbs); ok {
			return rel, true
		}
	}
	return "", false
}

func uriToAbs(uri string) (string, bool) {
	if uri == "" {
		return "", false
	}
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Scheme != "file" {
		return "", false
	}
	path := parsed.Path
	if path == "" {
		return "", false
	}
	return filepath.Clean(path), true
}

func relativeTo(root, abs string) (string, bool) {
	rel, err := filepath.Rel(filepath.Clean(root), abs)
	if err != nil {
		return "", false
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// PathToURI converts a workspace-relative slash path to a file:// URI.
func PathToURI(root, rel string) string {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	u := url.URL{Scheme: "file", Path: abs}
	return u.String()
}
