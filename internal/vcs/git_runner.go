package vcs

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/open-code-review/open-code-review/internal/gitcmd"
)

// GitRunner implements Runner for Git repositories.
type GitRunner struct {
	repoDir string
	ref     string // empty for workspace, otherwise a git ref
	runner  *gitcmd.Runner
}

// NewGitRunner creates a new GitRunner.
// ref is the git revision for file reads in range/commit mode; empty for workspace.
func NewGitRunner(repoDir, ref string, runner *gitcmd.Runner) *GitRunner {
	return &GitRunner{
		repoDir: repoDir,
		ref:     ref,
		runner:  runner,
	}
}

func (g *GitRunner) ReadFile(ctx context.Context, path string) (string, error) {
	if g.ref == "" {
		return g.readFromDisk(path)
	}
	return g.readFromGitShow(ctx, path)
}

func (g *GitRunner) ReadLines(ctx context.Context, path string, startLine, maxLines int) ([]string, int, error) {
	if g.ref == "" {
		return g.readLinesFromDisk(path, startLine, maxLines)
	}
	return g.readLinesFromGitShow(ctx, path, startLine, maxLines)
}

func (g *GitRunner) SearchText(ctx context.Context, searchText string, caseSensitive, usePerlRegexp bool, filePatterns []string) (string, error) {
	return gitGrep(ctx, g.repoDir, g.ref, searchText, caseSensitive, usePerlRegexp, filePatterns, g.runner)
}

func (g *GitRunner) ListFiles(ctx context.Context) ([]string, error) {
	return gitListFiles(ctx, g.repoDir, g.ref, g.runner)
}

func (g *GitRunner) readFromDisk(path string) (string, error) {
	fullPath, err := resolveWorkspacePath(g.repoDir, path)
	if err != nil {
		return "", err
	}
	content, err := os.ReadFile(fullPath)
	if err != nil {
		return "", fmt.Errorf("read file %q: %w", path, err)
	}
	return string(content), nil
}

func (g *GitRunner) readFromGitShow(parentCtx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer cancel()

	args := []string{"-c", "core.quotepath=false", "show", "--end-of-options", g.ref + ":" + path}
	if g.runner != nil {
		output, err := g.runner.Output(ctx, g.repoDir, args...)
		if err != nil {
			return "", fmt.Errorf("git show %s:%s: %w", g.ref, path, err)
		}
		return string(output), nil
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.repoDir
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git show %s:%s: %w", g.ref, path, err)
	}
	return string(output), nil
}

func (g *GitRunner) readLinesFromDisk(path string, startLine, maxLines int) ([]string, int, error) {
	fullPath, err := resolveWorkspacePath(g.repoDir, path)
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

func (g *GitRunner) readLinesFromGitShow(ctx context.Context, path string, startLine, maxLines int) ([]string, int, error) {
	args := []string{"-c", "core.quotepath=false", "show", "--end-of-options", g.ref + ":" + path}

	if g.runner != nil {
		var collected []string
		var totalLines int
		err := g.runner.Stream(ctx, g.repoDir, func(stdout io.Reader) error {
			var scanErr error
			collected, totalLines, scanErr = scanLines(stdout, startLine, maxLines)
			return scanErr
		}, args...)
		if err != nil {
			return nil, 0, fmt.Errorf("git show %s:%s: %w", g.ref, path, err)
		}
		return collected, totalLines, nil
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.repoDir
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 0, fmt.Errorf("git show %s:%s: %w", g.ref, path, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, 0, fmt.Errorf("git show %s:%s: %w", g.ref, path, err)
	}

	collected, totalLines, scanErr := scanLines(stdoutPipe, startLine, maxLines)
	if scanErr != nil {
		cmd.Process.Kill()
	}
	waitErr := cmd.Wait()

	if scanErr != nil {
		return nil, 0, fmt.Errorf("git show %s:%s: %w", g.ref, path, scanErr)
	}
	if waitErr != nil {
		return nil, 0, fmt.Errorf("git show %s:%s: %w", g.ref, path, waitErr)
	}
	return collected, totalLines, nil
}

func resolveWorkspacePath(repoDir, path string) (string, error) {
	repoRoot, err := filepath.Abs(repoDir)
	if err != nil {
		return "", fmt.Errorf("resolve repository path %q: %w", repoDir, err)
	}
	repoRoot, err = filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return "", fmt.Errorf("resolve repository path %q: %w", repoDir, err)
	}

	fullPath := filepath.Join(repoRoot, path)
	if !pathWithinBase(repoRoot, fullPath) {
		return "", fmt.Errorf("file path %q is outside repository", path)
	}

	resolvedPath, err := filepath.EvalSymlinks(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fullPath, nil
		}
		return "", fmt.Errorf("resolve file %q: %w", path, err)
	}
	if !pathWithinBase(repoRoot, resolvedPath) {
		return "", fmt.Errorf("file path %q is outside repository", path)
	}
	return resolvedPath, nil
}

func pathWithinBase(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

func scanLines(r io.Reader, startLine, maxLines int) ([]string, int, error) {
	br := bufio.NewReader(r)
	var collected []string
	lineNum := 0
	lastHadNewline := false

	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			lineNum++
			lastHadNewline = line[len(line)-1] == '\n'
			trimmed := strings.TrimSuffix(line, "\n")
			trimmed = strings.TrimSuffix(trimmed, "\r")
			if lineNum >= startLine && len(collected) < maxLines {
				collected = append(collected, trimmed)
			}
		}
		if err != nil {
			if err != io.EOF {
				return nil, 0, err
			}
			break
		}
	}

	if lastHadNewline {
		lineNum++
		if lineNum >= startLine && len(collected) < maxLines {
			collected = append(collected, "")
		}
	}

	return collected, lineNum, nil
}

const (
	gitGrepMaxCount = 100
	gitGrepTimeout  = 10 * time.Second
)

func gitGrep(ctx context.Context, repoDir, ref, searchText string, caseSensitive, usePerlRegexp bool, pathspec []string, runner *gitcmd.Runner) (string, error) {
	if strings.TrimSpace(searchText) == "" {
		return "Error: search_text is blank", nil
	}

	cmdArgs := []string{"--no-pager", "grep"}
	if !caseSensitive {
		cmdArgs = append(cmdArgs, "-i")
	}
	if usePerlRegexp {
		cmdArgs = append(cmdArgs, "-P")
	} else {
		cmdArgs = append(cmdArgs, "-F")
	}
	cmdArgs = append(cmdArgs, "-n", "--no-color")
	cmdArgs = append(cmdArgs, "--max-count", fmt.Sprintf("%d", gitGrepMaxCount))
	cmdArgs = append(cmdArgs, "-e", searchText)

	if ref != "" {
		cmdArgs = append(cmdArgs, "--end-of-options", ref, "--")
	} else {
		cmdArgs = append(cmdArgs, "--")
	}
	cmdArgs = append(cmdArgs, pathspec...)

	searchCtx, cancel := context.WithTimeout(ctx, gitGrepTimeout)
	defer cancel()

	var outStr, errStr string
	var err error
	if runner != nil {
		outStr, errStr, err = runner.RunSplit(searchCtx, repoDir, cmdArgs...)
	} else {
		cmd := exec.CommandContext(searchCtx, "git", cmdArgs...)
		cmd.Dir = repoDir
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err = cmd.Run()
		outStr = stdout.String()
		errStr = stderr.String()
	}

	if err != nil {
		if searchCtx.Err() != nil && err != nil {
			return "code_search timed out. Try narrowing file_patterns to a more specific path.", nil
		}
		if outStr == "" {
			if errStr == "" {
				return "No matches found", nil
			}
			return fmt.Sprintf("Error: %s", strings.TrimSpace(errStr)), nil
		}
	}

	lines := strings.Split(strings.TrimRight(outStr, "\n"), "\n")
	truncated := len(lines) >= gitGrepMaxCount

	type match struct {
		lineNum int
		content string
	}
	fileMatches := make(map[string][]match)
	var fileOrder []string
	seen := make(map[string]bool)

	hasRef := ref != ""
	splitN := 3
	offset := 0
	if hasRef {
		splitN = 4
		offset = 1
	}

	var sb strings.Builder
	if truncated {
		sb.WriteString(fmt.Sprintf("Note: The results have been truncated. Only showing first %d results.\n", gitGrepMaxCount))
	}

	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", splitN)
		if len(parts) < splitN {
			continue
		}
		fname := parts[offset]
		m := match{}
		lnVal, parseErr := parseInt(parts[offset+1])
		if parseErr != nil {
			continue
		}
		m.lineNum = lnVal
		m.content = parts[offset+2]
		if !seen[fname] {
			seen[fname] = true
			fileOrder = append(fileOrder, fname)
		}
		fileMatches[fname] = append(fileMatches[fname], m)
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

	return sb.String(), nil
}

func parseInt(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

const (
	gitFilesMaxCount = 100
	gitFilesTimeout  = 10 * time.Second
)

func gitListFiles(ctx context.Context, repoDir, ref string, runner *gitcmd.Runner) ([]string, error) {
	listCtx, cancel := context.WithTimeout(ctx, gitFilesTimeout)
	defer cancel()

	var args []string
	if ref != "" {
		args = []string{"ls-tree", "-r", "--name-only", "--end-of-options", ref}
	} else {
		args = []string{"ls-files", "--cached", "--others", "--exclude-standard"}
	}

	var output []byte
	var err error
	if runner != nil {
		output, err = runner.Output(listCtx, repoDir, args...)
	} else {
		cmd := exec.CommandContext(listCtx, "git", args...)
		cmd.Dir = repoDir
		output, err = cmd.Output()
	}

	if err != nil {
		if listCtx.Err() != nil {
			return nil, listCtx.Err()
		}
		return nil, err
	}

	var files []string
	lines := bytes.Split(bytes.TrimRight(output, "\n"), []byte{'\n'})
	for _, line := range lines {
		if len(line) > 0 {
			s := string(line)
			if shouldSkipGitFile(s) {
				continue
			}
			files = append(files, s)
		}
	}
	return files, nil
}

func shouldSkipGitFile(path string) bool {
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
