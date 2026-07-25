package protocol

// TextEdit is a computed change to one range of one file. Ranges are 0-based
// with UTF-16 characters, like everything else on the wire (SPEC §4.3).
type TextEdit struct {
	Range   Range  `json:"range"`
	NewText string `json:"new_text"`
}

// FileEdit groups the edits for a single file. Path is workspace-relative and
// slash-separated.
type FileEdit struct {
	Path  string     `json:"path"`
	Edits []TextEdit `json:"edits"`
}
