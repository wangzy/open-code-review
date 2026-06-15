package vcs

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Detect returns the VCS type for the given repository directory.
// It checks first for Perforce (via p4 client info), then for Git.
// If neither is detected, the error describes what went wrong.
func Detect(repoDir string) (Type, error) {
	if detectP4(repoDir) {
		return TypePerforce, nil
	}
	if detectGit(repoDir) {
		return TypeGit, nil
	}
	return "", fmt.Errorf("no VCS detected in %s", repoDir)
}

// IsGitRepo returns true if the directory is a Git repository.
func IsGitRepo(repoDir string) bool {
	return detectGit(repoDir)
}

// IsP4Client returns true if the directory is part of a Perforce workspace.
func IsP4Client(repoDir string) bool {
	return detectP4(repoDir)
}

func detectGit(repoDir string) bool {
	cmd := exec.Command("git", "-C", repoDir, "rev-parse", "--git-dir")
	cmd.Stderr = io.Discard
	return cmd.Run() == nil
}

func detectP4(repoDir string) bool {
	// Check if p4 command is available
	if _, err := exec.LookPath("p4"); err != nil {
		return false
	}

	// Check for .p4config or that the directory is under a p4 client
	// First try p4 info to see if we're in a valid client workspace
	cmd := exec.Command("p4", "info")
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		return false
	}

	// Look for "Client name:" in p4 info output
	if strings.Contains(string(out), "Client name:") {
		return true
	}

	// Fallback: check for .p4config file or p4config file
	matches, err := filepath.Glob(filepath.Join(repoDir, ".p4config"))
	if err == nil && len(matches) > 0 {
		return true
	}

	// Check if parent directories have .p4config
	dir := repoDir
	for {
		if _, err := os.Stat(filepath.Join(dir, ".p4config")); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return false
}

// P4ClientInfo returns the client name, port, and user from p4 info.
func P4ClientInfo(repoDir string) (client, port, user string, _ error) {
	return P4ClientInfoWithEnv(repoDir, nil)
}

// P4ClientInfoWithContext is like P4ClientInfo but accepts a context for timeout control.
func P4ClientInfoWithContext(ctx context.Context, repoDir string) (client, port, user string, _ error) {
	cmd := exec.CommandContext(ctx, "p4", "info")
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		return "", "", "", err
	}

	parseP4Info(string(out), &client, &port, &user)
	return client, port, user, nil
}

// P4ClientInfoWithEnv returns the client name, port, and user from p4 info,
// using the provided environment variable overrides.
func P4ClientInfoWithEnv(repoDir string, env []string) (client, port, user string, _ error) {
	cmd := exec.Command("p4", "info")
	cmd.Dir = repoDir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.Output()
	if err != nil {
		return "", "", "", err
	}

	parseP4Info(string(out), &client, &port, &user)
	return client, port, user, nil
}

func parseP4Info(output string, client, port, user *string) {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "Client name:") {
			*client = strings.TrimSpace(strings.TrimPrefix(line, "Client name:"))
		}
		if strings.HasPrefix(line, "Server address:") {
			*port = strings.TrimSpace(strings.TrimPrefix(line, "Server address:"))
		}
		if strings.HasPrefix(line, "User name:") {
			*user = strings.TrimSpace(strings.TrimPrefix(line, "User name:"))
		}
	}
}
