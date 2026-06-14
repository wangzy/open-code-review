package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func runGitCmd(repoDir string, args ...string) ([]byte, error) {
	fullArgs := append([]string{"-C", repoDir}, args...)
	cmd := exec.Command("git", fullArgs...)
	return cmd.CombinedOutput()
}

func getCommitMessage(repoDir, commit, p4Client, p4Port, p4User string) (string, error) {
	// Try git first
	out, err := runGitCmd(repoDir, "log", "-1", "--format=%B", "--end-of-options", commit)
	if err == nil && len(out) > 0 {
		return strings.TrimSpace(string(out)), nil
	}

	// Try Perforce describe for changelist
	if isNumeric(commit) {
		cmd := exec.Command("p4", "describe", "-s", commit)
		cmd.Dir = repoDir
		if p4Client != "" {
			cmd.Env = append(os.Environ(), "P4CLIENT="+p4Client)
		}
		if p4Port != "" {
			cmd.Env = append(cmd.Env, "P4PORT="+p4Port)
		}
		if p4User != "" {
			cmd.Env = append(cmd.Env, "P4USER="+p4User)
		}
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
