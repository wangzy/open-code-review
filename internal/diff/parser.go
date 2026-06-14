// Package diff parses unified diff output (both Git and Perforce formats)
// into structured Diff objects.
package diff

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/open-code-review/open-code-review/internal/gitcmd"
	"github.com/open-code-review/open-code-review/internal/model"
)

var (
	diffHeaderRe      = regexp.MustCompile(`^diff --git a/(.+?) b/(.+)$`)
	oldFileRe         = regexp.MustCompile(`^--- a/(.+)$`)
	newFileRe         = regexp.MustCompile(`^\+\+\+ b/(.+)$`)
	binaryRe          = regexp.MustCompile(`Binary files `)
	p4FileHeaderRe    = regexp.MustCompile(`^==== (.+?)#\d+(?:\s+\(\w+\))?\s*====$`)
	p4OldFileRe       = regexp.MustCompile(`^--- (.+?)(?:\t.+)?$`)
	p4NewFileRe       = regexp.MustCompile(`^\+\+\+ (.+?)(?:\t.+)?$`)
	p4ChangeHeaderRe  = regexp.MustCompile(`^Change \d+ by`)
)

// FileContentReader is a function that reads the full content of a file.
// It receives the file path and should return the content or an error.
type FileContentReader func(ctx context.Context, path string) (string, error)

// ParseDiffText splits the unified diff text into per-file Diff structs.
// ref, if non-empty, is a git ref used to read new-file content via
// git show instead of reading from the working tree.
// runner, if non-nil, is used to execute git subprocesses through a
// shared concurrency limiter.
func ParseDiffText(ctx context.Context, diffText string, repoDir string, ref string, runner *gitcmd.Runner) ([]model.Diff, error) {
	reader := func(ctx context.Context, path string) (string, error) {
		if ref != "" {
			args := []string{"-c", "core.quotepath=false", "show", "--end-of-options", ref + ":" + path}
			var output []byte
			var err error
			if runner != nil {
				output, err = runner.Output(ctx, repoDir, args...)
			} else {
				cmd := exec.CommandContext(ctx, "git", args...)
				cmd.Dir = repoDir
				output, err = cmd.Output()
			}
			if err != nil {
				return "", err
			}
			return string(output), nil
		}
		b, err := readWorkspaceFileForDiff(repoDir, path)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	return parseDiffTextWithReader(ctx, diffText, repoDir, reader)
}

// ParseDiffTextWithVCS splits VCS diff text into per-file Diff structs.
// reader is called to obtain the full file content for each non-deleted file.
// It auto-detects the diff format (Git or Perforce) from the header patterns.
func ParseDiffTextWithVCS(ctx context.Context, diffText string, repoDir string, reader FileContentReader) ([]model.Diff, error) {
	return parseDiffTextWithReader(ctx, diffText, repoDir, reader)
}

func parseDiffTextWithReader(ctx context.Context, diffText string, repoDir string, reader FileContentReader) ([]model.Diff, error) {
	lines := strings.Split(diffText, "\n")
	var diffs []model.Diff
	var current *model.Diff
	var buf strings.Builder

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	for _, line := range lines {
		// Git header: diff --git a/path b/path
		if m := diffHeaderRe.FindStringSubmatch(line); m != nil {
			if current != nil {
				current.Diff = strings.TrimSuffix(buf.String(), "\n")
				finalizeDiffWithReader(ctx, current, repoDir, reader)
				diffs = append(diffs, *current)
				buf.Reset()
			}
			current = &model.Diff{
				OldPath: m[1],
				NewPath: m[2],
			}
		}

		// P4 header: ==== //depot/path#rev (type) ====
		if current == nil {
			if m := p4FileHeaderRe.FindStringSubmatch(line); m != nil {
				depotPath := m[1]
				relPath := depotPathToRelative(depotPath)
				current = &model.Diff{
					OldPath: relPath,
					NewPath: relPath,
				}
			}
		}

		if current == nil {
			continue
		}

		switch {
		case binaryRe.MatchString(line):
			current.IsBinary = true
		// Extended header lines (unambiguous: content lines always carry a
		// leading "+", "-" or " " prefix, so a bare prefix match is safe).
		case strings.HasPrefix(line, "new file mode "):
			current.IsNew = true
		case strings.HasPrefix(line, "deleted file mode "):
			current.IsDeleted = true
		case strings.HasPrefix(line, "rename from "):
			// Authoritative old path for renames; more reliable than the
			// "diff --git" header when paths contain spaces.
			current.OldPath = strings.TrimPrefix(line, "rename from ")
			current.IsRenamed = true
		case strings.HasPrefix(line, "rename to "):
			current.NewPath = strings.TrimPrefix(line, "rename to ")
			current.IsRenamed = true
		// Git old-file: --- a/path
		case oldFileRe.MatchString(line):
			if p := oldFileRe.FindStringSubmatch(line); len(p) > 1 && p[1] == "/dev/null" {
				current.IsNew = true
			}
		// P4 old-file: --- //depot/path\trev
		case p4OldFileRe.MatchString(line) && !oldFileRe.MatchString(line):
			if p := p4OldFileRe.FindStringSubmatch(line); len(p) > 1 {
				p4Path := strings.TrimSpace(p[1])
				if p4Path == "/dev/null" {
					current.IsNew = true
				}
			}
		// Git new-file: +++ b/path
		case newFileRe.MatchString(line):
			if p := newFileRe.FindStringSubmatch(line); len(p) > 1 && p[1] == "/dev/null" {
				current.IsDeleted = true
			}
		// P4 new-file: +++ //depot/path\trev
		case p4NewFileRe.MatchString(line) && !newFileRe.MatchString(line):
			if p := p4NewFileRe.FindStringSubmatch(line); len(p) > 1 {
				p4Path := strings.TrimSpace(p[1])
				if p4Path == "/dev/null" {
					current.IsDeleted = true
				}
			}
		// git emits "--- /dev/null" / "+++ /dev/null" without a/ b/ prefixes.
		case line == "--- /dev/null":
			current.IsNew = true
		case line == "+++ /dev/null":
			current.IsDeleted = true
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			current.Insertions++
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") && !strings.HasPrefix(line, "--"):
			current.Deletions++
		}
		buf.WriteString(line)
		buf.WriteString("\n")
	}

	if current != nil {
		current.Diff = strings.TrimSuffix(buf.String(), "\n")
		finalizeDiffWithReader(ctx, current, repoDir, reader)
		diffs = append(diffs, *current)
	}

	return diffs, nil
}

func finalizeDiffWithReader(ctx context.Context, d *model.Diff, repoDir string, reader FileContentReader) {
	if d.IsDeleted || d.NewPath == "/dev/null" {
		d.NewPath = "/dev/null"
		return
	}
	content, err := reader(ctx, d.NewPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ocr] WARNING: cannot read file %s: %v\n", d.NewPath, err)
		return
	}
	d.NewFileContent = content
}

func depotPathToRelative(depotPath string) string {
	if strings.HasPrefix(depotPath, "//") {
		trimmed := depotPath[2:]
		if idx := strings.Index(trimmed, "/"); idx >= 0 {
			return trimmed[idx+1:]
		}
		return trimmed
	}
	return depotPath
}



// DefaultFileReader creates a simple FileContentReader that reads from disk or git show.
func DefaultFileReader(ctx context.Context, repoDir, path, ref string, runner *gitcmd.Runner) (string, error) {
	if ref != "" {
		args := []string{"-c", "core.quotepath=false", "show", "--end-of-options", ref + ":" + path}
		var output []byte
		var err error
		if runner != nil {
			output, err = runner.Output(ctx, repoDir, args...)
		} else {
			cmd := exec.CommandContext(ctx, "git", args...)
			cmd.Dir = repoDir
			output, err = cmd.Output()
		}
		if err != nil {
			return "", err
		}
		return string(output), nil
	}
	content, err := readWorkspaceFileForDiff(repoDir, path)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

// IsP4DiffFormat returns true if the diff text appears to be Perforce format.
func IsP4DiffFormat(diffText string) bool {
	if p4FileHeaderRe.MatchString(diffText) {
		return true
	}
	if strings.Contains(diffText, "\n==== ") {
		return true
	}
	if p4ChangeHeaderRe.MatchString(diffText) {
		return true
	}
	return false
}
