package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Diagnostics describe, but never repair or accept, malformed model output.
// Offset is one-based in bytes of the trimmed JSON; line/column are one-based
// with Unicode columns. Keep the original error available via errors.As/Is.
type jsonContractError struct {
	Offset       int64
	Line, Column int
	Snippet      string
	Cause        error
}

func (e *jsonContractError) Error() string {
	return fmt.Sprintf("%v；JSON byte_offset=%d, line=%d, column=%d；附近文本（仅作定位）=%q",
		e.Cause, e.Offset, e.Line, e.Column, e.Snippet)
}

func (e *jsonContractError) Unwrap() error { return e.Cause }

func describeJSONError(text string, err error) error {
	var syntax *json.SyntaxError
	var mismatch *json.UnmarshalTypeError
	var offset int64
	switch {
	case errors.As(err, &syntax):
		offset = syntax.Offset
	case errors.As(err, &mismatch):
		offset = mismatch.Offset
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		offset = int64(len(text) + 1)
	default:
		return err // Unknown-field errors already contain the field name.
	}
	pos := int(offset) - 1
	if pos < 0 {
		pos = 0
	}
	if pos > len(text) {
		pos = len(text)
	}
	prefix := text[:pos]
	line := strings.Count(prefix, "\n") + 1
	column := utf8.RuneCountInString(prefix[strings.LastIndex(prefix, "\n")+1:]) + 1
	before, after := []rune(prefix), []rune(text[pos:])
	if len(before) > 48 {
		before = before[len(before)-48:]
	}
	if len(after) > 48 {
		after = after[:48]
	}
	return &jsonContractError{Offset: offset, Line: line, Column: column,
		Snippet: string(before) + "⟦错误位置⟧" + string(after), Cause: err}
}
