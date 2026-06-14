// Package vcs provides an abstraction layer over version control systems (Git, Perforce).
// It defines a Runner interface for file content operations, allowing the code-review tools
// (file_read, code_search, file_find) to work across different VCS backends.
package vcs

import "context"

// Type identifies a version control system.
type Type string

const (
	TypeGit      Type = "git"
	TypePerforce Type = "p4"
)

// Mode defines how the diff is retrieved.
type Mode int

const (
	ModeWorkspace Mode = iota // current working state (uncommitted / pending changelist)
	ModeCommit                // single commit / changelist
	ModeRange                 // range between two refs / changelists
)

// RefInfo describes the current review target for a VCS backend.
type RefInfo struct {
	Mode Mode
	// From/To/Commit are the original user-supplied values (branch names, changelists, etc.).
	From   string
	To     string
	Commit string
}

// ResolveRef returns the revision to use for file-read operations based on mode:
// - ModeWorkspace: empty (read from disk)
// - ModeCommit: Commit
// - ModeRange: To
func (ri RefInfo) ResolveRef() string {
	switch ri.Mode {
	case ModeCommit:
		return ri.Commit
	case ModeRange:
		return ri.To
	default:
		return ""
	}
}

// Runner abstracts VCS-specific operations for reading files, searching code,
// and listing files. All methods are safe for concurrent use.
type Runner interface {
	// ReadFile reads the full content of a file at the given ref.
	// In workspace mode ref is empty and the file is read from disk.
	ReadFile(ctx context.Context, path string) (string, error)

	// ReadLines reads a window of lines from a file.
	// startLine is 1-based. Returns the collected lines and total line count.
	ReadLines(ctx context.Context, path string, startLine, maxLines int) ([]string, int, error)

	// SearchText searches for text across repository files.
	// Returns a formatted string with matching file:line entries.
	SearchText(ctx context.Context, searchText string, caseSensitive, usePerlRegexp bool, filePatterns []string) (string, error)

	// ListFiles returns all tracked + relevant file paths in the repository.
	ListFiles(ctx context.Context) ([]string, error)
}
