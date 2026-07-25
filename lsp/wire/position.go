package wire

import (
	"strings"
	"unicode/utf16"
)

// UTF16Column converts a byte offset within line into a UTF-16 code-unit column
// (SPEC §4.3). A non-BMP rune counts as 2.
//
// This function and ByteOffset are the ONLY converters in the tree. Everything
// that computes a column from bytes goes through them; get this wrong and a
// query silently resolves a different symbol rather than failing
// (testdata/README.md §1.5).
func UTF16Column(line string, byteOffset int) int {
	if byteOffset <= 0 {
		return 0
	}
	if byteOffset > len(line) {
		byteOffset = len(line)
	}
	col := 0
	for i, r := range line {
		if i >= byteOffset {
			break
		}
		col += utf16.RuneLen(r)
	}
	return col
}

// ByteOffset is the inverse of UTF16Column: it returns the byte index in line of
// the given UTF-16 code-unit column.
//
// A column landing inside a surrogate pair is not a legal LSP position; it
// snaps to the start of that rune, which is the only answer that cannot corrupt
// the string.
func ByteOffset(line string, utf16Column int) int {
	if utf16Column <= 0 {
		return 0
	}
	col := 0
	for i, r := range line {
		if col >= utf16Column {
			return i
		}
		next := col + utf16.RuneLen(r)
		if next > utf16Column {
			// The column lands inside this rune's surrogate pair. Snapping to
			// the rune's first byte is the only answer that cannot split a
			// character in half.
			return i
		}
		col = next
	}
	return len(line)
}

// LineIndex maps a file's content to its lines, for preview extraction and
// UTF-16 conversion. It is built once per file and reused.
type LineIndex struct {
	lines []string
}

// NewLineIndex splits content into lines, tolerating both LF and CRLF.
func NewLineIndex(content string) LineIndex {
	if content == "" {
		return LineIndex{}
	}
	lines := strings.Split(content, "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	return LineIndex{lines: lines}
}

// Line returns line n (0-based), or "" when n is out of range. Out-of-range is
// not an error: a language server may legitimately report a position in a file
// that changed under us, and a missing preview must never fail a query.
func (l LineIndex) Line(n int) string {
	if n < 0 || n >= len(l.lines) {
		return ""
	}
	return l.lines[n]
}

// Len reports the number of lines.
func (l LineIndex) Len() int { return len(l.lines) }
