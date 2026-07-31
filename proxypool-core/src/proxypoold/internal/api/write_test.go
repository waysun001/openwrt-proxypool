package api

import (
	"errors"
	"testing"
)

type shortWriter struct {
	limit  int
	writes int
}

func (w *shortWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.limit == 0 {
		return 0, nil
	}
	if len(p) > w.limit {
		return w.limit, nil
	}
	return len(p), nil
}

func TestWriteAllCompletesShortWrites(t *testing.T) {
	w := &shortWriter{limit: 2}
	if err := writeAll(w, []byte("abcdef")); err != nil {
		t.Fatal(err)
	}
	if w.writes != 3 {
		t.Fatalf("writes=%d want 3", w.writes)
	}
}
func TestWriteAllRejectsZeroProgress(t *testing.T) {
	if err := writeAll(&shortWriter{}, []byte("x")); !errors.Is(err, errNoWriteProgress) {
		t.Fatalf("error=%v", err)
	}
}

func TestNormalizeResponseRejectsInvalidPresenceCombinations(t *testing.T) {
	cases := []Response{
		{Result: []byte(`{}`), Error: &Error{}},
		{Result: []byte(`{}`), Error: &Error{Code: "x"}},
		{Result: []byte(`{`), Error: &Error{Code: "x", Message: "x"}},
		{},
	}
	for _, input := range cases {
		if got := normalizeResponse(input, "id"); got.Error == nil || got.Error.Code != "internal_error" || got.Result != nil {
			t.Fatal("invalid response was accepted")
		}
	}
}
