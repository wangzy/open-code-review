package main

import (
	"fmt"
	"os/exec"
	"strings"
)

func runGitCmd(repoDir string, args ...string) ([]byte, error) {
	fullArgs := append([]string{"-C", repoDir}, args...)
	cmd := exec.Command("git", fullArgs...)
	return cmd.CombinedOutput()
}

func getCommitMessage(repoDir, commit string) (string, error) {
	// Try git first
	out, err := runGitCmd(repoDir, "log", "-1", "--format=%B", "--end-of-options", commit)
	if err == nil && len(out) > 0 {
		return strings.TrimSpace(string(out)), nil
	}

	// Try Perforce describe for changelist
	if isNumeric(commit) {
		cmd := exec.Command("p4", "describe", "-s", commit)
		cmd.Dir = repoDir
		out, err := cmd.Output()
		if err == nil && len(out) > 0 {
			return strings.TrimSpace(string(out)), nil
		}
	}

	return "", fmt.Errorf("cannot get commit message for %s", commit)
}

func isNumeric(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}
