package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/open-code-review/open-code-review/internal/agent"
	"github.com/open-code-review/open-code-review/internal/config/rules"
	"github.com/open-code-review/open-code-review/internal/config/template"
	"github.com/open-code-review/open-code-review/internal/config/toolsconfig"
	"github.com/open-code-review/open-code-review/internal/diff"
	"github.com/open-code-review/open-code-review/internal/gitcmd"
	"github.com/open-code-review/open-code-review/internal/llm"
	"github.com/open-code-review/open-code-review/internal/stdout"
	"github.com/open-code-review/open-code-review/internal/telemetry"
	"github.com/open-code-review/open-code-review/internal/tool"
	"github.com/open-code-review/open-code-review/internal/vcs"
)

func runReview(args []string) error {
	opts, err := parseReviewFlags(args)
	if err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if opts.showHelp {
		printReviewUsage()
		return nil
	}

	if err := requireRepo(opts.repoDir, opts.vcsType); err != nil {
		return err
	}

	tpl, err := template.LoadDefault()
	if err != nil {
		return fmt.Errorf("load default template: %w", err)
	}
	if opts.maxTools > 0 {
		tpl.MaxToolRequestTimes = opts.maxTools
	}
	if err := tpl.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	repoDir, err := resolveRepoDir(opts.repoDir, opts.vcsType)
	if err != nil {
		return fmt.Errorf("resolve repo: %w", err)
	}
	if err := validateReviewRefs(repoDir, opts); err != nil {
		return err
	}

	if opts.commit != "" && opts.background == "" {
		if msg, err := getCommitMessage(repoDir, opts.commit, opts.p4Client, opts.p4Port, opts.p4User); err == nil && msg != "" {
			opts.background = msg
		}
	}

	resolver, fileFilter, err := rules.NewResolver(repoDir, opts.rulePath)
	if err != nil {
		return fmt.Errorf("load rules: %w", err)
	}

	if opts.preview {
		return runPreview(repoDir, opts, fileFilter)
	}

	toolEntries, err := toolsconfig.Load(opts.toolConfigPath)
	if err != nil {
		return fmt.Errorf("load tools: %w", err)
	}
	planToolDefs := agent.BuildToolDefs(toolEntries, true)
	mainToolDefs := agent.BuildToolDefs(toolEntries, false)

	cfgPath, err := defaultConfigPath()
	if err != nil {
		return err
	}

	appCfg, err := LoadAppConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("load app config: %w", err)
	}
	if appCfg != nil {
		tpl.ApplyLanguage(appCfg.Language)
	}

	ep, err := llm.ResolveEndpoint(cfgPath)
	if err != nil {
		return fmt.Errorf("resolve LLM endpoint: %w", err)
	}

	llmClient := llm.NewLLMClient(ep)
	model := ep.Model

	gitRunner := gitcmd.New(opts.maxGitProcs)

	vcstype := resolveVCSType(opts.vcsType, repoDir)
	mode := tool.ParseReviewMode(opts.from, opts.to, opts.commit)

	var vcsRunner vcs.Runner
	switch vcstype {
	case vcs.TypePerforce:
		// P4Runner uses a single changelist number for file reads (p4 print).
		// For workspace mode ref stays empty; for commit/range modes the target
		// changelist is used. The --from changelist is only needed during diff
		// generation, not for file content reads.
		var ref string
		if opts.commit != "" {
			ref = opts.commit
		} else if opts.to != "" {
			ref = opts.to
		}
		vcsRunner = vcs.NewP4Runner(repoDir, ref, opts.p4Client, opts.p4Port, opts.p4User)
	default:
		// Ref for file reading: to ref for range mode, commit for commit mode, empty for workspace.
		var ref string
		switch mode {
		case tool.ModeRange:
			ref = opts.to
		case tool.ModeCommit:
			ref = opts.commit
		}
		vcsRunner = vcs.NewGitRunner(repoDir, ref, gitRunner)
	}

	collector := tool.NewCommentCollector()
	fileReader := tool.NewFileReader(repoDir, vcsRunner)

	tools := buildToolRegistry(collector, fileReader)

	ag := agent.New(agent.Args{
		RepoDir:               repoDir,
		From:                  opts.from,
		To:                    opts.to,
		Commit:                opts.commit,
		Template:              *tpl,
		SystemRule:            resolver,
		FileFilter:            fileFilter,
		LLMClient:             llmClient,
		Tools:                 tools,
		PlanToolDefs:          planToolDefs,
		MainToolDefs:          mainToolDefs,
		CommentCollector:      collector,
		CommentWorkerPool:     agent.NewCommentWorkerPool(opts.concurrency),
		MaxConcurrency:        opts.concurrency,
		ConcurrentTaskTimeout: opts.perFileTimeout,
		Model:                 model,
		Background:            opts.background,
		GitRunner:             gitRunner,
		VCSType:               vcstype,
		VCSRunner:             vcsRunner,
		P4Client:              opts.p4Client,
		P4Port:                opts.p4Port,
		P4User:                opts.p4User,
	})

	// Silence progress output during execution; restore before Summary in agent mode.
	var unsilence func()
	if opts.outputFormat == "json" || opts.audience == "agent" {
		unsilence = stdout.Quiet()
		defer func() {
			if unsilence != nil {
				unsilence()
			}
		}()
	}

	ctx, span := telemetry.StartSpan(context.Background(), "review.run")
	defer span.End()
	startTime := time.Now()

	comments, err := ag.Run(ctx)
	if err != nil {
		telemetry.SetAttr(span, "error", err.Error())
		return fmt.Errorf("review failed: %w", err)
	}

	// Resolve line numbers by matching existing_code against diff hunks.
	comments = diff.ResolveLineNumbers(comments, ag.Diffs())

	// Record summary metrics (files_reviewed is refined by agent.Run).
	duration := time.Since(startTime)
	telemetry.RecordReviewDuration(ctx, duration)
	if len(comments) > 0 {
		telemetry.RecordCommentsGenerated(ctx, int64(len(comments)))
	}

	// If no files were reviewed (e.g. workspace has no changes), inform the caller in JSON mode.
	if opts.outputFormat == "json" && len(comments) == 0 && ag.FilesReviewed() == 0 {
		return outputJSONNoFiles()
	}

	// In agent mode (text output), restore stdout so Summary reaches the terminal.
	if opts.audience == "agent" && opts.outputFormat != "json" && unsilence != nil {
		unsilence()
		unsilence = nil
	}

	if opts.outputFormat != "json" {
		telemetry.PrintTraceSummary(ag.FilesReviewed(), int64(len(comments)), ag.TotalInputTokens(), ag.TotalOutputTokens(), ag.TotalTokensUsed(), ag.TotalCacheReadTokens(), ag.TotalCacheWriteTokens(), duration)
	}

	if opts.outputFormat == "json" {
		return outputJSONWithWarnings(comments, ag.Warnings(), ag.FilesReviewed(), ag.TotalInputTokens(), ag.TotalOutputTokens(), ag.TotalTokensUsed(), ag.TotalCacheReadTokens(), ag.TotalCacheWriteTokens(), duration)
	}
	if opts.audience == "agent" {
		outputTextWithWarnings(comments, ag.Warnings())
		return nil
	}
	outputTextWithWarnings(comments, ag.Warnings())

	return nil
}

func resolveRepoDir(input string, vcsType string) (string, error) {
	if input == "" {
		var err error
		input, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("get working directory: %w", err)
		}
	}
	absPath, err := filepath.Abs(input)
	if err != nil {
		return "", fmt.Errorf("resolve absolute path: %w", err)
	}

	// Try auto-detection when vcsType is not explicitly set
	if vcsType == "" {
		vcsType = string(resolveVCSType("", absPath))
	}

	if vcsType == "p4" {
		return resolveP4RepoDir(absPath)
	}
	return resolveGitRepoDir(absPath)
}

func resolveGitRepoDir(absPath string) (string, error) {
	out, err := runGitCmd(absPath, "rev-parse", "--git-dir")
	if err != nil || len(out) == 0 {
		return "", fmt.Errorf("%s is not a git repository", absPath)
	}
	return absPath, nil
}

func resolveP4RepoDir(absPath string) (string, error) {
	client, _, _, err := vcs.P4ClientInfo(absPath)
	if err != nil {
		return "", fmt.Errorf("%s is not a Perforce workspace: %w", absPath, err)
	}
	if client == "" {
		return "", fmt.Errorf("%s is not a Perforce workspace (no client found)", absPath)
	}
	return absPath, nil
}

// resolveVCSType determines the VCS type from the flag value and auto-detection.
func resolveVCSType(flagValue string, repoDir string) vcs.Type {
	switch flagValue {
	case "p4":
		return vcs.TypePerforce
	case "git":
		return vcs.TypeGit
	default:
		t, err := vcs.Detect(repoDir)
		if err != nil {
			return vcs.TypeGit // default to Git if auto-detection fails
		}
		return t
	}
}

// requireRepo validates that the given directory is a valid repository of the given VCS type.
func requireRepo(dir string, vcsType string) error {
	repoDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	if vcsType == "p4" {
		return nil // P4 repos are validated later in resolveRepoDir
	}
	out, err := runGitCmd(repoDir, "rev-parse", "--git-dir")
	if err != nil || len(out) == 0 {
		return fmt.Errorf("%s is not a git repository, code review requires a valid git repository", repoDir)
	}
	return nil
}

func validateReviewRefs(repoDir string, opts reviewOptions) error {
	// For Perforce, refs are changelist numbers — skip git rev-parse validation.
	if opts.vcsType == "p4" || (opts.vcsType == "" && vcs.IsP4Client(repoDir)) {
		return validateP4Refs(opts)
	}

	refs := []struct {
		flag string
		ref  string
	}{
		{"--from", opts.from},
		{"--to", opts.to},
		{"--commit", opts.commit},
	}
	for _, item := range refs {
		if item.ref == "" {
			continue
		}
		if strings.HasPrefix(item.ref, "-") {
			return fmt.Errorf("%s value %q is not a valid git ref: refs must not start with '-'", item.flag, item.ref)
		}
		if out, err := runGitCmd(repoDir, "rev-parse", "--verify", "--end-of-options", item.ref+"^{commit}"); err != nil {
			msg := strings.TrimSpace(string(out))
			if msg != "" {
				return fmt.Errorf("%s value %q is not a valid commit ref: %s", item.flag, item.ref, msg)
			}
			return fmt.Errorf("%s value %q is not a valid commit ref", item.flag, item.ref)
		}
	}
	return nil
}

func validateP4Refs(opts reviewOptions) error {
	refs := []struct {
		flag string
		ref  string
	}{
		{"--from", opts.from},
		{"--to", opts.to},
		{"--commit", opts.commit},
	}
	for _, item := range refs {
		if item.ref == "" {
			continue
		}
		// Changelist numbers must be numeric
		for _, c := range item.ref {
			if c < '0' || c > '9' {
				return fmt.Errorf("%s value %q is not a valid changelist number", item.flag, item.ref)
			}
		}
	}
	return nil
}

func runPreview(repoDir string, opts reviewOptions, fileFilter *rules.FileFilter) error {
	gitRunner := gitcmd.New(opts.maxGitProcs)
	ag := agent.New(agent.Args{
		RepoDir:    repoDir,
		From:       opts.from,
		To:         opts.to,
		Commit:     opts.commit,
		FileFilter: fileFilter,
		GitRunner:  gitRunner,
	})

	preview, err := ag.Preview(context.Background())
	if err != nil {
		return fmt.Errorf("preview failed: %w", err)
	}

	outputPreviewText(preview)
	return nil
}

func buildToolRegistry(collector *tool.CommentCollector, fr *tool.FileReader) *tool.Registry {
	reg := tool.NewRegistry()
	reg.Register(tool.NewFileRead(fr))
	reg.Register(tool.NewFileFind(fr))
	reg.Register(tool.NewFileReadDiff(tool.DiffMap{}))
	reg.Register(tool.NewCodeSearch(fr.Runner))
	reg.Register(&tool.CodeCommentProvider{Collector: collector})
	return reg
}
