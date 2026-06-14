package vcs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// P4Runner implements Runner for Perforce repositories.
type P4Runner struct {
	repoDir  string // client workspace root
	ref      string // changelist number for range/commit mode; empty for workspace
	client   string // P4CLIENT
	port     string // P4PORT
	user     string // P4USER
}

// NewP4Runner creates a new P4Runner.
// ref is the changelist number for file reads in range/commit mode; empty for workspace.
func NewP4Runner(repoDir, ref, client, port, user string) *P4Runner {
	return &P4Runner{
		repoDir: repoDir,
		ref:     ref,
		client:  client,
		port:    port,
		user:    user,
	}
}

func (p *P4Runner) env() []string {
	var env []string
	if p.client != "" {
		env = append(env, "P4CLIENT="+p.client)
	}
	if p.port != "" {
		env = append(env, "P4PORT="+p.port)
	}
	if p.user != "" {
		env = append(env, "P4USER="+p.user)
	}
	return env
}

func (p *P4Runner) runP4(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "p4", args...)
	cmd.Dir = p.repoDir
	if env := p.env(); len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("p4 %s: %w: %s", strings.Join(args, " "), err, string(out))
	}
	return string(out), nil
}

func (p *P4Runner) runP4Output(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "p4", args...)
	cmd.Dir = p.repoDir
	if env := p.env(); len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	return cmd.Output()
}

func (p *P4Runner) ReadFile(ctx context.Context, path string) (string, error) {
	if p.ref == "" {
		return p.readFromDisk(path)
	}
	return p.readFromP4Print(ctx, path)
}

func (p *P4Runner) ReadLines(ctx context.Context, path string, startLine, maxLines int) ([]string, int, error) {
	if p.ref == "" {
		return p.readLinesFromDisk(path, startLine, maxLines)
	}
	return p.readLinesFromP4Print(ctx, path, startLine, maxLines)
}

func (p *P4Runner) SearchText(ctx context.Context, searchText string, caseSensitive, usePerlRegexp bool, filePatterns []string) (string, error) {
	return p4Grep(ctx, p.repoDir, p.env(), p.ref, searchText, caseSensitive, usePerlRegexp, filePatterns)
}

func (p *P4Runner) ListFiles(ctx context.Context) ([]string, error) {
	return p4ListFiles(ctx, p.repoDir, p.env(), p.ref)
}

func (p *P4Runner) readFromDisk(path string) (string, error) {
	fullPath, err := resolveWorkspacePath(p.repoDir, path)
	if err != nil {
		return "", err
	}
	content, err := os.ReadFile(fullPath)
	if err != nil {
		return "", fmt.Errorf("read file %q: %w", path, err)
	}
	return string(content), nil
}

func (p *P4Runner) readFromP4Print(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	fileSpec := fmt.Sprintf("%s@=%s", sanitizeP4Path(path), p.ref)
	out, err := p.runP4(ctx, "print", "-q", fileSpec)
	if err != nil {
		return "", fmt.Errorf("p4 print %s: %w", fileSpec, err)
	}
	return out, nil
}

func (p *P4Runner) readLinesFromDisk(path string, startLine, maxLines int) ([]string, int, error) {
	fullPath, err := resolveWorkspacePath(p.repoDir, path)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(fullPath)
	if err != nil {
		return nil, 0, fmt.Errorf("read file %q: %w", path, err)
	}
	defer f.Close()
	return scanLines(f, startLine, maxLines)
}

func (p *P4Runner) readLinesFromP4Print(ctx context.Context, path string, startLine, maxLines int) ([]string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	fileSpec := fmt.Sprintf("%s@=%s", sanitizeP4Path(path), p.ref)
	cmd := exec.CommandContext(ctx, "p4", "print", "-q", fileSpec)
	cmd.Dir = p.repoDir
	if env := p.env(); len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 0, fmt.Errorf("p4 print %s: %w", fileSpec, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, 0, fmt.Errorf("p4 print %s: %w", fileSpec, err)
	}

	collected, totalLines, scanErr := scanLines(stdoutPipe, startLine, maxLines)
	if scanErr != nil {
		cmd.Process.Kill()
	}
	waitErr := cmd.Wait()

	if scanErr != nil {
		return nil, 0, fmt.Errorf("p4 print %s: %w", fileSpec, scanErr)
	}
	if waitErr != nil {
		return nil, 0, fmt.Errorf("p4 print %s: %w", fileSpec, waitErr)
	}
	return collected, totalLines, nil
}

const (
	p4GrepMaxCount = 100
	p4GrepTimeout  = 30 * time.Second
	p4MaxFiles     = 500
	p4FilesTimeout = 30 * time.Second
)

// sanitizeP4Path escapes Perforce special characters in file paths
// to prevent revision-spec injection (@, #).
func sanitizeP4Path(path string) string {
	s := strings.ReplaceAll(path, "%", "%25")
	s = strings.ReplaceAll(s, "@", "%40")
	s = strings.ReplaceAll(s, "#", "%23")
	return s
}

func p4Grep(ctx context.Context, repoDir string, env []string, ref, searchText string, caseSensitive, usePerlRegexp bool, filePatterns []string) (string, error) {
	if strings.TrimSpace(searchText) == "" {
		return "Error: search_text is blank", nil
	}

	ctx, cancel := context.WithTimeout(ctx, p4GrepTimeout)
	defer cancel()

	args := []string{"grep", "-n", "-e", searchText}
	if !caseSensitive {
		args = append(args, "-i")
	}
	// p4 grep supports -F for fixed strings
	if !usePerlRegexp {
		args = append(args, "-F")
	}

	if ref != "" {
		args = append(args, "@="+ref)
	}

	// Add file patterns as path restrictions
	for _, pattern := range filePatterns {
		if strings.TrimSpace(pattern) != "" {
			args = append(args, sanitizeP4Path(pattern))
		}
	}

	cmd := exec.CommandContext(ctx, "p4", args...)
	cmd.Dir = repoDir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	outStr := stdout.String()
	errStr := stderr.String()

	if err != nil {
		if ctx.Err() != nil {
			return "code_search timed out. Try narrowing file_patterns to a more specific path.", nil
		}
		if outStr == "" {
			if errStr == "" {
				return "No matches found", nil
			}
			return fmt.Sprintf("Error: %s", strings.TrimSpace(errStr)), nil
		}
	}

	return formatP4GrepOutput(repoDir, outStr, err, errStr), nil
}

func formatP4GrepOutput(repoDir, outStr string, err error, errStr string) string {
	lines := strings.Split(strings.TrimRight(outStr, "\n"), "\n")

	type match struct {
		lineNum int
		content string
	}
	fileMatches := make(map[string][]match)
	var fileOrder []string
	seen := make(map[string]bool)

	truncated := false
	if len(lines) > p4GrepMaxCount {
		lines = lines[:p4GrepMaxCount]
		truncated = true
	}

	var sb strings.Builder
	if truncated {
		sb.WriteString(fmt.Sprintf("Note: The results have been truncated. Only showing first %d results.\n", p4GrepMaxCount))
	}

	for _, line := range lines {
		if line == "" {
			continue
		}
		// p4 grep output: //depot/path/file.cpp:123:content
		firstColon := strings.Index(line, ":")
		if firstColon < 0 {
			continue
		}
		fname := line[:firstColon]
		rest := line[firstColon+1:]

		secondColon := strings.Index(rest, ":")
		if secondColon < 0 {
			continue
		}
		lineNumStr := rest[:secondColon]
		content := rest[secondColon+1:]

		ln, err := strconv.Atoi(lineNumStr)
		if err != nil {
			continue
		}

		// Convert depot path to relative path if possible
		relPath := depotPathToRelative(repoDir, fname)

		if !seen[relPath] {
			seen[relPath] = true
			fileOrder = append(fileOrder, relPath)
		}
		fileMatches[relPath] = append(fileMatches[relPath], match{lineNum: ln, content: content})
	}

	for _, path := range fileOrder {
		matches := fileMatches[path]
		sb.WriteString(fmt.Sprintf("File: %s\nMatch lines: %d\n", path, len(matches)))
		for _, m := range matches {
			sb.WriteString(fmt.Sprintf("%d|%s\n", m.lineNum, m.content))
		}
		sb.WriteString("\n")
	}

	if err != nil && errStr != "" {
		sb.WriteString(fmt.Sprintf("Warning: %s\n", strings.TrimSpace(errStr)))
	}

	return sb.String()
}

func p4ListFiles(ctx context.Context, repoDir string, env []string, ref string) ([]string, error) {
	listCtx, cancel := context.WithTimeout(ctx, p4FilesTimeout)
	defer cancel()

	var args []string
	if ref != "" {
		// For a specific changelist, use p4 files revision spec (without -e flag)
		args = []string{"files", fmt.Sprintf("@=%s,@=%s", ref, ref)}
	} else {
		// For workspace, use p4 have to list files in the current workspace
		args = []string{"have"}
	}

	cmd := exec.CommandContext(listCtx, "p4", args...)
	cmd.Dir = repoDir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}

	output, err := cmd.Output()
	if err != nil {
		if listCtx.Err() != nil {
			return nil, listCtx.Err()
		}
		return nil, err
	}

	var files []string
	lines := bytes.Split(bytes.TrimRight(output, "\n"), []byte{'\n'})
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		// p4 have output: //depot/path/file.cpp#5 - /local/path/file.cpp
		// p4 files output: //depot/path/file.cpp#5 - action change 12345 (type)
		s := string(line)
		path := extractP4FilePath(s)
		if path == "" {
			continue
		}
		relPath := depotPathToRelative(repoDir, path)
		if shouldSkipP4File(relPath) {
			continue
		}
		files = append(files, relPath)
		if len(files) >= p4MaxFiles {
			break
		}
	}
	return files, nil
}

func extractP4FilePath(line string) string {
	// p4 have: //depot/path/file.cpp#5 - /local/path/file.cpp
	// Split on " - " to get depot path + local path parts
	parts := strings.SplitN(line, " - ", 2)
	if len(parts) == 0 {
		return ""
	}

	// Get the depot path part: //depot/path/file.cpp#5
	depotPart := strings.TrimSpace(parts[0])
	// Strip revision: //depot/path/file.cpp#5 -> //depot/path/file.cpp
	if idx := strings.LastIndex(depotPart, "#"); idx >= 0 {
		depotPart = depotPart[:idx]
	}
	// p4 files may have " - action change 12345" format
	// If we have a local path, prefer it from p4 have output
	if len(parts) > 1 {
		localPart := strings.TrimSpace(parts[1])
		// Check if it looks like a local path (starts with /)
		if strings.HasPrefix(localPart, "/") && !strings.HasPrefix(localPart, "//") {
			return localPart
		}
	}
	return depotPart
}

func depotPathToRelative(repoDir, depotPath string) string {
	// If it's already a local absolute path, make it relative to repoDir
	if filepath.IsAbs(depotPath) {
		rel, err := filepath.Rel(repoDir, depotPath)
		if err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
		return depotPath
	}
	// For depot paths (//depot/...), strip the depot prefix as a best-effort fallback.
	// A proper implementation would use p4 where to translate depot paths.
	if strings.HasPrefix(depotPath, "//") {
		trimmed := depotPath[2:]
		// Skip past the depot name if separated by /
		if idx := strings.Index(trimmed, "/"); idx >= 0 {
			trimmed = trimmed[idx+1:]
		}
		return trimmed
	}
	return depotPath
}

func shouldSkipP4File(path string) bool {
	base := path
	if idx := strings.LastIndex(path, "/"); idx != -1 {
		base = path[idx+1:]
	}
	hasExt := strings.Contains(base, ".")
	if !hasExt {
		switch base {
		case "Makefile", "Dockerfile", "LICENSE", "Vagrantfile", "Containerfile":
			return false
		}
		return true
	}
	return false
}

// P4Diff generates a unified diff for Perforce changes.
// mode: ModeWorkspace (pending/default CL), ModeCommit (single CL), ModeRange (between two CLs).
func P4Diff(ctx context.Context, repoDir string, env []string, mode Mode, from, to, commit string) (string, error) {
	switch mode {
	case ModeCommit:
		return p4DescribeChangelist(ctx, repoDir, env, commit)
	case ModeRange:
		// For range mode, generate diffs between two changelists
		// This requires enumerating files changed between them and getting diffs
		return p4DiffRange(ctx, repoDir, env, from, to)
	case ModeWorkspace:
		return p4WorkspaceDiff(ctx, repoDir, env)
	default:
		return "", fmt.Errorf("unknown p4 diff mode: %d", mode)
	}
}

func p4DescribeChangelist(ctx context.Context, repoDir string, env []string, cl string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "p4", "describe", "-du", cl)
	cmd.Dir = repoDir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("p4 describe -du %s: %w: %s", cl, err, string(out))
	}
	return string(out), nil
}

func p4WorkspaceDiff(ctx context.Context, repoDir string, env []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "p4", "diff", "-du")
	cmd.Dir = repoDir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("p4 diff -du: %w: %s", err, string(out))
	}
	return string(out), nil
}

func p4DiffRange(ctx context.Context, repoDir string, env []string, fromCL, toCL string) (string, error) {
	// TODO: Implement properly by enumerating changed files between CLs
	// and diffing only those, rather than diffing the entire depot.
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	// Use p4 diff2 with -u flag to compare two changelists
	// p4 diff2 -u //...@=fromCL //...@=toCL
	cmd := exec.CommandContext(ctx, "p4", "diff2", "-u")
	cmd.Dir = repoDir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}

	// We need to add file patterns. For now, diff all files between two changelists.
	// A more targeted approach would be to get the list of changed files first.
	cmd.Args = append(cmd.Args,
		fmt.Sprintf("//...@=%s", fromCL),
		fmt.Sprintf("//...@=%s", toCL),
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("p4 diff2: %w: %s", err, string(out))
	}
	return string(out), nil
}
