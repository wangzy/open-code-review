package tool

import (
	"context"
	"fmt"
	"strings"
)

// CodeSearchProvider performs text search across the repository.
type CodeSearchProvider struct {
	runner vcsRunner
}

type vcsRunner interface {
	SearchText(ctx context.Context, searchText string, caseSensitive, usePerlRegexp bool, filePatterns []string) (string, error)
}

func NewCodeSearch(r vcsRunner) *CodeSearchProvider { return &CodeSearchProvider{runner: r} }

func (p *CodeSearchProvider) Tool() Tool { return CodeSearch }

func (p *CodeSearchProvider) Execute(ctx context.Context, args map[string]any) (string, error) {
	searchText, _ := args["search_text"].(string)
	caseSensitive, _ := args["case_sensitive"].(bool)
	usePerlRegexp, _ := args["use_perl_regexp"].(bool)

	filePatternsIface, _ := args["file_patterns"].([]any)
	var patterns []string
	for _, item := range filePatternsIface {
		if s, ok := item.(string); ok && s != "" {
			patterns = append(patterns, s)
		}
	}

	if strings.TrimSpace(searchText) == "" {
		return "Error: search_text is blank", nil
	}

	result, err := p.runner.SearchText(ctx, searchText, caseSensitive, usePerlRegexp, patterns)
	if err != nil {
		return "", fmt.Errorf("code_search failed: %w", err)
	}
	return result, nil
}
