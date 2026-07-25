package wire_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fingerskier/langer/lsp/wire"
)

func TestUTF16ColumnASCII(t *testing.T) {
	t.Parallel()
	const line = "export function getUserById(id: string): User {"
	for _, tc := range []struct{ byteOffset, want int }{
		{0, 0},
		{7, 7},
		{16, 16},
		{len(line), len(line)},
	} {
		if got := wire.UTF16Column(line, tc.byteOffset); got != tc.want {
			t.Errorf("UTF16Column(byte %d) = %d, want %d", tc.byteOffset, got, tc.want)
		}
	}
}

// U+1F680 ROCKET: 1 codepoint, 2 UTF-16 code units, 4 UTF-8 bytes.
func TestUTF16ColumnCountsNonBMPAsTwo(t *testing.T) {
	t.Parallel()
	const line = "🚀🚀x"
	if got, want := wire.UTF16Column(line, 0), 0; got != want {
		t.Errorf("before the first rocket: %d, want %d", got, want)
	}
	if got, want := wire.UTF16Column(line, 4), 2; got != want {
		t.Errorf("after one rocket: %d, want %d", got, want)
	}
	if got, want := wire.UTF16Column(line, 8), 4; got != want {
		t.Errorf("after two rockets: %d, want %d", got, want)
	}
	if got, want := wire.UTF16Column(line, 9), 5; got != want {
		t.Errorf("after the trailing x: %d, want %d", got, want)
	}
}

// A BMP multi-byte rune (é, 2 UTF-8 bytes) is still ONE UTF-16 code unit. This
// is the case a naive len([]rune) conversion gets right and a naive byte
// conversion gets wrong.
func TestUTF16ColumnBMPMultibyte(t *testing.T) {
	t.Parallel()
	const line = "café x"
	if got, want := wire.UTF16Column(line, len("café")), 4; got != want {
		t.Errorf("UTF16Column after \"café\" = %d, want %d", got, want)
	}
}

func TestByteOffsetIsTheInverse(t *testing.T) {
	t.Parallel()
	lines := []string{
		"plain ascii line",
		"🚀🚀🚀🚀🚀🚀🚀🚀 tail",
		"café 🚀 naïve",
		"",
	}
	for _, line := range lines {
		for byteOffset := 0; byteOffset <= len(line); byteOffset++ {
			if byteOffset < len(line) && !utf8Boundary(line, byteOffset) {
				continue
			}
			col := wire.UTF16Column(line, byteOffset)
			if got := wire.ByteOffset(line, col); got != byteOffset {
				t.Errorf("line %q: ByteOffset(UTF16Column(%d)=%d) = %d, want %d",
					line, byteOffset, col, got, byteOffset)
			}
		}
	}
}

func utf8Boundary(s string, i int) bool {
	return s[i]&0xC0 != 0x80
}

func TestByteOffsetClampsOutOfRange(t *testing.T) {
	t.Parallel()
	const line = "abc"
	if got, want := wire.ByteOffset(line, 99), len(line); got != want {
		t.Errorf("ByteOffset past the end = %d, want %d", got, want)
	}
	if got, want := wire.ByteOffset(line, -1), 0; got != want {
		t.Errorf("ByteOffset(-1) = %d, want %d", got, want)
	}
	if got, want := wire.UTF16Column(line, 99), 3; got != want {
		t.Errorf("UTF16Column past the end = %d, want %d", got, want)
	}
}

// A column landing INSIDE a surrogate pair is not a legal position; snapping to
// the start of the rune is the only non-corrupting answer.
func TestByteOffsetInsideSurrogatePairSnapsToRuneStart(t *testing.T) {
	t.Parallel()
	const line = "🚀x"
	if got, want := wire.ByteOffset(line, 1), 0; got != want {
		t.Errorf("ByteOffset mid-surrogate = %d, want %d", got, want)
	}
}

// THE authoritative M1 UTF-16 assertion, tied to the real fixture file rather
// than to a hand-written string: testdata/README.md §1.5 states that
// `getUserById` on src/unicode.ts line 5 starts at UTF-16 character 69, and
// that the codepoint (61) and byte (85) readings are both wrong.
func TestUTF16ColumnAgreesWithTheTypeScriptFixture(t *testing.T) {
	t.Parallel()
	line := fixtureLine(t, filepath.Join("ts-project", "src", "unicode.ts"), 5)

	call := strings.Index(line, "getUserById(\"42\")")
	if call < 0 {
		t.Fatalf("fixture line does not contain the expected call: %q", line)
	}
	if got, want := wire.UTF16Column(line, call), 69; got != want {
		t.Errorf("getUserById UTF-16 column = %d, want %d (testdata/README.md §1.5)", got, want)
	}
	if got, want := len([]rune(line[:call])), 61; got != want {
		t.Errorf("codepoint column = %d, want the documented wrong answer %d", got, want)
	}
	if got, want := call, 85; got != want {
		t.Errorf("byte column = %d, want the documented wrong answer %d", got, want)
	}

	rockets := strings.Index(line, "ROCKETS")
	if got, want := wire.UTF16Column(line, rockets), 13; got != want {
		t.Errorf("ROCKETS UTF-16 column = %d, want %d", got, want)
	}
	rocketName := strings.Index(line, "rocketName")
	if got, want := wire.UTF16Column(line, rocketName), 56; got != want {
		t.Errorf("rocketName UTF-16 column = %d, want %d", got, want)
	}
	if got, want := wire.ByteOffset(line, 69), call; got != want {
		t.Errorf("ByteOffset(69) = %d, want %d", got, want)
	}
}

// The Python twin: testdata/README.md §2.5.
func TestUTF16ColumnAgreesWithThePythonFixture(t *testing.T) {
	t.Parallel()
	line := fixtureLine(t, filepath.Join("py-project", "unicode_positions.py"), 9)

	call := strings.Index(line, "get_user_by_id(\"42\")")
	if call < 0 {
		t.Fatalf("fixture line does not contain the expected call: %q", line)
	}
	if got, want := wire.UTF16Column(line, call), 44; got != want {
		t.Errorf("get_user_by_id UTF-16 column = %d, want %d (testdata/README.md §2.5)", got, want)
	}
	if got, want := len([]rune(line[:call])), 36; got != want {
		t.Errorf("codepoint column = %d, want the documented wrong answer %d", got, want)
	}
	if got, want := call, 60; got != want {
		t.Errorf("byte column = %d, want the documented wrong answer %d", got, want)
	}

	name := strings.Index(line, "rocket_name")
	if got, want := wire.UTF16Column(line, name), 30; got != want {
		t.Errorf("rocket_name UTF-16 column = %d, want %d", got, want)
	}
}

func fixtureLine(t *testing.T, rel string, n int) string {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", rel)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading fixture %s: %v", path, err)
	}
	lines := strings.Split(string(data), "\n")
	if n >= len(lines) {
		t.Fatalf("fixture %s has %d lines, wanted line %d", path, len(lines), n)
	}
	return strings.TrimSuffix(lines[n], "\r")
}

func TestLineIndex(t *testing.T) {
	t.Parallel()
	idx := wire.NewLineIndex("alpha\nbeta\r\ngamma")
	for n, want := range map[int]string{0: "alpha", 1: "beta", 2: "gamma"} {
		if got := idx.Line(n); got != want {
			t.Errorf("Line(%d) = %q, want %q", n, got, want)
		}
	}
	if got := idx.Line(99); got != "" {
		t.Errorf("Line past the end = %q, want \"\"", got)
	}
	if got := idx.Line(-1); got != "" {
		t.Errorf("Line(-1) = %q, want \"\"", got)
	}
}

func TestLineIndexOnFixture(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", "testdata", "ts-project", "src", "user.ts")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	idx := wire.NewLineIndex(string(data))
	if got, want := idx.Line(5), "export function getUserById(id: string): User {"; got != want {
		t.Errorf("Line(5) = %q, want %q", got, want)
	}
}
