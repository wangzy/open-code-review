package tool

import (
	"context"

	"github.com/open-code-review/open-code-review/internal/vcs"
)

// ReviewMode represents the active review mode.
type ReviewMode int

const (
	ModeWorkspace ReviewMode = iota
	ModeRange
	ModeCommit
)

// ParseReviewMode returns the correct ReviewMode based on provided flag values.
func ParseReviewMode(from, to, commit string) ReviewMode {
	if commit != "" {
		return ModeCommit
	}
	if from != "" && to != "" {
		return ModeRange
	}
	return ModeWorkspace
}

// FileReader resolves file contents using a VCS Runner.
type FileReader struct {
	RepoDir string
	Runner  vcs.Runner
}

// NewFileReader creates a FileReader backed by the given VCS Runner.
func NewFileReader(repoDir string, runner vcs.Runner) *FileReader {
	return &FileReader{
		RepoDir: repoDir,
		Runner:  runner,
	}
}

// Read returns the full content of a file path (relative to RepoDir),
// resolved via the VCS Runner.
func (fr *FileReader) Read(ctx context.Context, path string) (string, error) {
	return fr.Runner.ReadFile(ctx, path)
}

// ReadLines returns a window of lines from the file plus the total line count.
// startLine is 1-based; maxLines is the maximum number of lines to collect.
func (fr *FileReader) ReadLines(ctx context.Context, path string, startLine, maxLines int) ([]string, int, error) {
	return fr.Runner.ReadLines(ctx, path, startLine, maxLines)
}

// Ref returns an empty string for the new interface (backward compat).
// This method is kept for code that checks ref but it's no longer the source of truth.
func (fr *FileReader) Ref() string {
	return ""
}
