package vcs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Detect returns the VCS type for the given repository directory.
// It checks first for Perforce (via p4 client info), then for Git.
// Returns an error if neither VCS is detected.
func Detect(repoDir string) Type {
	if detectP4(repoDir) {
		return TypePerforce
	}
	if detectGit(repoDir) {
		return TypeGit
	}
	return TypeGit // default to Git if uncertain
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
	cmd.Stderr = nil
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
	matches, _ := filepath.Glob(filepath.Join(repoDir, ".p4config"))
	if len(matches) > 0 {
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
	cmd := exec.Command("p4", "info")
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		return "", "", "", err
	}

	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "Client name:") {
			client = strings.TrimSpace(strings.TrimPrefix(line, "Client name:"))
		}
		if strings.HasPrefix(line, "Server address:") {
			port = strings.TrimSpace(strings.TrimPrefix(line, "Server address:"))
		}
		if strings.HasPrefix(line, "User name:") {
			user = strings.TrimSpace(strings.TrimPrefix(line, "User name:"))
		}
	}
	return client, port, user, nil
}
