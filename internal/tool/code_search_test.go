package tool

import (
	"context"
	"testing"
)

func TestCodeSearchExecute_BlankSearchText(t *testing.T) {
	p := NewCodeSearch(&stubRunner{})
	result, err := p.Execute(context.Background(), map[string]any{
		"search_text": "   ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result != "Error: search_text is blank" {
		t.Errorf("expected blank error, got: %s", result)
	}
}

func TestCodeSearchExecute_EmptyFilePatterns(t *testing.T) {
	p := NewCodeSearch(&stubRunner{})
	result, err := p.Execute(context.Background(), map[string]any{
		"search_text":   "myFunc",
		"file_patterns": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result != "not found" {
		t.Errorf("expected 'not found', got: %s", result)
	}
}

type stubRunner struct{}

func (s *stubRunner) SearchText(ctx context.Context, searchText string, caseSensitive, usePerlRegexp bool, filePatterns []string) (string, error) {
	return "not found", nil
}
