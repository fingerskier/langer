// Package protocol holds the shared domain types and the structured error
// model used across every boundary in langer: the MCP tool surface (SPEC §4.4),
// the daemon IPC protocol (SPEC §3.5), and the SQLite index.
//
// Two rules govern everything in this package:
//
//   - JSON shapes are the contract. They are pinned verbatim against SPEC §4.4
//     by TestSpecResultShapes. Changing a tag breaks the spec, not the test.
//   - Positions are 0-based lines and characters, and characters are UTF-16
//     code units (SPEC §4.3). No other encoding is negotiated in v0.1.
package protocol

// Position is a 0-based location inside a document. Character is an offset in
// UTF-16 code units, matching the LSP default (SPEC §4.3).
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is a half-open span between two positions.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location is the result shape of get_definition and get_references
// (SPEC §4.4). Path is workspace-relative.
type Location struct {
	Path         string `json:"path"`
	Range        Range  `json:"range"`
	IsDefinition bool   `json:"is_definition"`
	Preview      string `json:"preview,omitempty"`
}

// Hover is the result shape of get_hover (SPEC §4.4).
type Hover struct {
	Contents      string `json:"contents"`
	Documentation string `json:"documentation,omitempty"`
	Range         *Range `json:"range,omitempty"`
}

// Symbol is the result shape of document_symbols and workspace_symbols
// (SPEC §4.4).
type Symbol struct {
	Name      string     `json:"name"`
	Kind      SymbolKind `json:"kind"`
	Container string     `json:"container,omitempty"`
	Path      string     `json:"path"`
	Range     Range      `json:"range"`
	Detail    string     `json:"detail,omitempty"`
}

// Diagnostic is the result shape of get_diagnostics (SPEC §4.4).
type Diagnostic struct {
	Path     string   `json:"path"`
	Severity Severity `json:"severity"`
	Code     string   `json:"code,omitempty"`
	Source   string   `json:"source,omitempty"`
	Message  string   `json:"message"`
	Range    Range    `json:"range"`
}

// SymbolKind is the lowercase, language-neutral spelling of an LSP SymbolKind.
// SPEC §4.4 shows "function"; the rest follow the same convention.
type SymbolKind string

// Symbol kinds, mirroring the LSP SymbolKind enumeration (1..26).
const (
	SymbolKindUnknown       SymbolKind = "unknown"
	SymbolKindFile          SymbolKind = "file"
	SymbolKindModule        SymbolKind = "module"
	SymbolKindNamespace     SymbolKind = "namespace"
	SymbolKindPackage       SymbolKind = "package"
	SymbolKindClass         SymbolKind = "class"
	SymbolKindMethod        SymbolKind = "method"
	SymbolKindProperty      SymbolKind = "property"
	SymbolKindField         SymbolKind = "field"
	SymbolKindConstructor   SymbolKind = "constructor"
	SymbolKindEnum          SymbolKind = "enum"
	SymbolKindInterface     SymbolKind = "interface"
	SymbolKindFunction      SymbolKind = "function"
	SymbolKindVariable      SymbolKind = "variable"
	SymbolKindConstant      SymbolKind = "constant"
	SymbolKindString        SymbolKind = "string"
	SymbolKindNumber        SymbolKind = "number"
	SymbolKindBoolean       SymbolKind = "boolean"
	SymbolKindArray         SymbolKind = "array"
	SymbolKindObject        SymbolKind = "object"
	SymbolKindKey           SymbolKind = "key"
	SymbolKindNull          SymbolKind = "null"
	SymbolKindEnumMember    SymbolKind = "enum_member"
	SymbolKindStruct        SymbolKind = "struct"
	SymbolKindEvent         SymbolKind = "event"
	SymbolKindOperator      SymbolKind = "operator"
	SymbolKindTypeParameter SymbolKind = "type_parameter"
)

// lspSymbolKinds indexes SymbolKind by the LSP wire value (index 0 unused).
var lspSymbolKinds = [...]SymbolKind{
	SymbolKindUnknown, // 0 — not a valid LSP value
	SymbolKindFile,
	SymbolKindModule,
	SymbolKindNamespace,
	SymbolKindPackage,
	SymbolKindClass,
	SymbolKindMethod,
	SymbolKindProperty,
	SymbolKindField,
	SymbolKindConstructor,
	SymbolKindEnum,
	SymbolKindInterface,
	SymbolKindFunction,
	SymbolKindVariable,
	SymbolKindConstant,
	SymbolKindString,
	SymbolKindNumber,
	SymbolKindBoolean,
	SymbolKindArray,
	SymbolKindObject,
	SymbolKindKey,
	SymbolKindNull,
	SymbolKindEnumMember,
	SymbolKindStruct,
	SymbolKindEvent,
	SymbolKindOperator,
	SymbolKindTypeParameter,
}

// SymbolKindFromLSP converts an LSP SymbolKind integer to its spelling.
// Values outside 1..26 map to SymbolKindUnknown rather than being dropped, so a
// language server extension never costs us the symbol.
func SymbolKindFromLSP(kind int) SymbolKind {
	if kind <= 0 || kind >= len(lspSymbolKinds) {
		return SymbolKindUnknown
	}
	return lspSymbolKinds[kind]
}

// Severity is the diagnostic severity spelling used in SPEC §4.4 ("error").
type Severity string

// Diagnostic severities, mirroring the LSP DiagnosticSeverity enumeration.
const (
	SeverityError       Severity = "error"
	SeverityWarning     Severity = "warning"
	SeverityInformation Severity = "information"
	SeverityHint        Severity = "hint"
)

// SeverityFromLSP converts an LSP DiagnosticSeverity to its spelling. LSP
// leaves an absent severity up to the client; we treat it (and anything
// unrecognised) as an error so nothing silently drops below the agent's notice.
func SeverityFromLSP(severity int) Severity {
	switch severity {
	case 2:
		return SeverityWarning
	case 3:
		return SeverityInformation
	case 4:
		return SeverityHint
	default:
		return SeverityError
	}
}
