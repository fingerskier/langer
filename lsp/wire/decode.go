package wire

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// RawPosition is an LSP position: 0-based line, UTF-16 code-unit character.
type RawPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// RawRange is an LSP range.
type RawRange struct {
	Start RawPosition `json:"start"`
	End   RawPosition `json:"end"`
}

// RawLocation is a decoded Location or LocationLink, reduced to the one range
// that matters.
type RawLocation struct {
	URI   string
	Range RawRange
}

// RawHover is a decoded Hover with its markup flattened to a single blob.
type RawHover struct {
	Value string
	Range *RawRange
}

// RawSymbol is a decoded DocumentSymbol or SymbolInformation.
//
// URI is set only for SymbolInformation, whose symbols carry their own file —
// which is what workspace/symbol returns.
type RawSymbol struct {
	Name              string
	Detail            string
	Kind              int
	Range             RawRange
	SelectionRange    RawRange
	HasSelectionRange bool
	Container         string
	URI               string
	Children          []RawSymbol
}

// RawDiagnostic is one decoded diagnostic, carrying the URI of its publish.
type RawDiagnostic struct {
	URI      string
	Range    RawRange
	Severity int
	Code     string
	Source   string
	Message  string
}

// RawTextEdit is one decoded TextEdit.
type RawTextEdit struct {
	Range   RawRange
	NewText string
}

// RawFileEdit groups a file's edits from a WorkspaceEdit.
type RawFileEdit struct {
	URI   string
	Edits []RawTextEdit
}

// ---- textDocument/definition ----

type jsonLocation struct {
	URI   string   `json:"uri"`
	Range RawRange `json:"range"`
}

type jsonLocationLink struct {
	TargetURI            string    `json:"targetUri"`
	TargetRange          RawRange  `json:"targetRange"`
	TargetSelectionRange *RawRange `json:"targetSelectionRange"`
}

// DecodeDefinition handles Location | Location[] | LocationLink[] | null.
// typescript-language-server returns LocationLink; pyright returns Location.
func DecodeDefinition(raw json.RawMessage) ([]RawLocation, error) {
	if isNullOrEmpty(raw) {
		return nil, nil
	}

	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "{") {
		var one jsonLocation
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, fmt.Errorf("decoding Location: %w", err)
		}
		return []RawLocation{{URI: one.URI, Range: one.Range}}, nil
	}
	if !strings.HasPrefix(trimmed, "[") {
		return nil, fmt.Errorf("definition result is neither an object nor an array: %s", clip(trimmed))
	}

	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("decoding definition array: %w", err)
	}

	out := make([]RawLocation, 0, len(items))
	for _, item := range items {
		// A LocationLink has targetUri; a Location has uri. Probe rather than
		// guess by server, because the same server answers differently for
		// different requests.
		var probe struct {
			URI       string `json:"uri"`
			TargetURI string `json:"targetUri"`
		}
		if err := json.Unmarshal(item, &probe); err != nil {
			return nil, fmt.Errorf("decoding definition element: %w", err)
		}
		switch {
		case probe.TargetURI != "":
			var link jsonLocationLink
			if err := json.Unmarshal(item, &link); err != nil {
				return nil, fmt.Errorf("decoding LocationLink: %w", err)
			}
			rng := link.TargetRange
			if link.TargetSelectionRange != nil {
				// The identifier, not the whole declaration.
				rng = *link.TargetSelectionRange
			}
			out = append(out, RawLocation{URI: link.TargetURI, Range: rng})
		case probe.URI != "":
			var loc jsonLocation
			if err := json.Unmarshal(item, &loc); err != nil {
				return nil, fmt.Errorf("decoding Location: %w", err)
			}
			out = append(out, RawLocation{URI: loc.URI, Range: loc.Range})
		default:
			return nil, fmt.Errorf("definition element has neither uri nor targetUri: %s", clip(string(item)))
		}
	}
	return out, nil
}

// DecodeLocations decodes a plain Location[] (textDocument/references).
func DecodeLocations(raw json.RawMessage) ([]RawLocation, error) {
	return DecodeDefinition(raw)
}

// ---- textDocument/hover ----

// DecodeHover handles MarkedString | MarkedString[] | MarkupContent, and the
// null / empty forms both servers use for "no hover here".
func DecodeHover(raw json.RawMessage) (*RawHover, error) {
	if isNullOrEmpty(raw) {
		return nil, nil
	}
	var envelope struct {
		Contents json.RawMessage `json:"contents"`
		Range    *RawRange       `json:"range"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("decoding Hover: %w", err)
	}

	value, err := decodeMarkup(envelope.Contents)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	return &RawHover{Value: value, Range: envelope.Range}, nil
}

// decodeMarkup flattens every legal `contents` shape into one markdown blob.
func decodeMarkup(raw json.RawMessage) (string, error) {
	if isNullOrEmpty(raw) {
		return "", nil
	}
	trimmed := strings.TrimSpace(string(raw))

	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", fmt.Errorf("decoding hover string: %w", err)
		}
		return s, nil

	case '{':
		var obj struct {
			Kind     string `json:"kind"`
			Language string `json:"language"`
			Value    string `json:"value"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			return "", fmt.Errorf("decoding hover object: %w", err)
		}
		if obj.Language != "" {
			// MarkedString{language, value} is a code block by definition.
			return "```" + obj.Language + "\n" + obj.Value + "\n```", nil
		}
		return obj.Value, nil

	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return "", fmt.Errorf("decoding hover array: %w", err)
		}
		parts := make([]string, 0, len(items))
		for _, item := range items {
			part, err := decodeMarkup(item)
			if err != nil {
				return "", err
			}
			if strings.TrimSpace(part) != "" {
				parts = append(parts, part)
			}
		}
		return strings.Join(parts, "\n\n"), nil

	default:
		return "", fmt.Errorf("hover contents is neither string, object nor array: %s", clip(trimmed))
	}
}

// ---- textDocument/documentSymbol and workspace/symbol ----

// DecodeSymbols handles DocumentSymbol[] (hierarchical) and
// SymbolInformation[] (flat). Both are legal answers to the same request; the
// discriminator is the presence of a `location` key.
func DecodeSymbols(raw json.RawMessage) ([]RawSymbol, error) {
	if isNullOrEmpty(raw) {
		return nil, nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, "[") {
		return nil, fmt.Errorf("symbol result is not an array: %s", clip(trimmed))
	}

	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("decoding symbol array: %w", err)
	}

	out := make([]RawSymbol, 0, len(items))
	for _, item := range items {
		sym, err := decodeSymbol(item)
		if err != nil {
			return nil, err
		}
		out = append(out, sym)
	}
	return out, nil
}

type jsonSymbol struct {
	Name           string            `json:"name"`
	Detail         string            `json:"detail"`
	Kind           int               `json:"kind"`
	Range          *RawRange         `json:"range"`
	SelectionRange *RawRange         `json:"selectionRange"`
	ContainerName  string            `json:"containerName"`
	Location       *jsonLocation     `json:"location"`
	Children       []json.RawMessage `json:"children"`
}

func decodeSymbol(raw json.RawMessage) (RawSymbol, error) {
	var js jsonSymbol
	if err := json.Unmarshal(raw, &js); err != nil {
		return RawSymbol{}, fmt.Errorf("decoding symbol: %w", err)
	}

	sym := RawSymbol{
		Name:      js.Name,
		Detail:    js.Detail,
		Kind:      js.Kind,
		Container: js.ContainerName,
	}

	switch {
	case js.Location != nil: // SymbolInformation
		sym.URI = js.Location.URI
		sym.Range = js.Location.Range
		sym.SelectionRange = js.Location.Range
	case js.Range != nil: // DocumentSymbol
		sym.Range = *js.Range
		if js.SelectionRange != nil {
			sym.SelectionRange = *js.SelectionRange
			sym.HasSelectionRange = true
		} else {
			sym.SelectionRange = *js.Range
		}
	default:
		return RawSymbol{}, fmt.Errorf("symbol %q has neither range nor location", js.Name)
	}

	for _, child := range js.Children {
		decoded, err := decodeSymbol(child)
		if err != nil {
			return RawSymbol{}, err
		}
		sym.Children = append(sym.Children, decoded)
	}
	return sym, nil
}

// ---- textDocument/publishDiagnostics ----

// DecodeCode renders LSP's integer|string Diagnostic.code as a string,
// VERBATIM. pyright sends "reportAttributeAccessIssue"; TypeScript sends the
// integer 2339, which becomes "2339". No prefix is synthesised (docs §10.5).
func DecodeCode(raw json.RawMessage) string {
	if isNullOrEmpty(raw) {
		return ""
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
		return ""
	}
	// Numeric. json.Number keeps the literal so 2339 never becomes "2.339e+03".
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	// Structured code (LSP 3.16 allows {value, target}); take the value.
	var structured struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &structured); err == nil && len(structured.Value) > 0 {
		return DecodeCode(structured.Value)
	}
	return ""
}

// DecodePublishDiagnostics decodes a textDocument/publishDiagnostics payload,
// returning the diagnostics and the URI they belong to.
func DecodePublishDiagnostics(raw json.RawMessage) ([]RawDiagnostic, string, error) {
	if isNullOrEmpty(raw) {
		return nil, "", nil
	}
	var params struct {
		URI         string `json:"uri"`
		Diagnostics []struct {
			Range    RawRange        `json:"range"`
			Severity int             `json:"severity"`
			Code     json.RawMessage `json:"code"`
			Source   string          `json:"source"`
			Message  string          `json:"message"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, "", fmt.Errorf("decoding publishDiagnostics: %w", err)
	}

	out := make([]RawDiagnostic, 0, len(params.Diagnostics))
	for _, d := range params.Diagnostics {
		out = append(out, RawDiagnostic{
			URI:      params.URI,
			Range:    d.Range,
			Severity: d.Severity,
			Code:     DecodeCode(d.Code),
			Source:   d.Source,
			Message:  d.Message,
		})
	}
	return out, params.URI, nil
}

// ---- textDocument/rename ----

// DecodeWorkspaceEdit handles both WorkspaceEdit representations: the `changes`
// map and the `documentChanges` array.
func DecodeWorkspaceEdit(raw json.RawMessage) ([]RawFileEdit, error) {
	if isNullOrEmpty(raw) {
		return nil, nil
	}
	var edit struct {
		Changes         map[string][]RawTextEditJSON `json:"changes"`
		DocumentChanges []struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
			Edits []RawTextEditJSON `json:"edits"`
		} `json:"documentChanges"`
	}
	if err := json.Unmarshal(raw, &edit); err != nil {
		return nil, fmt.Errorf("decoding WorkspaceEdit: %w", err)
	}

	var out []RawFileEdit
	// documentChanges wins when both are present: LSP says it is the richer
	// form and a server that sends both keeps them consistent.
	if len(edit.DocumentChanges) > 0 {
		for _, dc := range edit.DocumentChanges {
			if dc.TextDocument.URI == "" {
				continue
			}
			out = append(out, RawFileEdit{URI: dc.TextDocument.URI, Edits: toTextEdits(dc.Edits)})
		}
		return out, nil
	}

	uris := make([]string, 0, len(edit.Changes))
	for uri := range edit.Changes {
		uris = append(uris, uri)
	}
	sortStrings(uris) // map iteration order must not leak into a result
	for _, uri := range uris {
		out = append(out, RawFileEdit{URI: uri, Edits: toTextEdits(edit.Changes[uri])})
	}
	return out, nil
}

// RawTextEditJSON is the wire shape of a TextEdit.
type RawTextEditJSON struct {
	Range   RawRange `json:"range"`
	NewText string   `json:"newText"`
}

func toTextEdits(in []RawTextEditJSON) []RawTextEdit {
	out := make([]RawTextEdit, 0, len(in))
	for _, e := range in {
		out = append(out, RawTextEdit{Range: e.Range, NewText: e.NewText})
	}
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// clip shortens a payload for an error message.
func clip(s string) string {
	const limit = 120
	if len(s) <= limit {
		return strconv.Quote(s)
	}
	return strconv.Quote(s[:limit]) + "…"
}
