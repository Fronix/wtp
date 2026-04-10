package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/satococoa/wtp/v2/internal/command"
	"github.com/satococoa/wtp/v2/internal/config"
	"github.com/satococoa/wtp/v2/internal/errors"
	"github.com/satococoa/wtp/v2/internal/git"
	wtpio "github.com/satococoa/wtp/v2/internal/io"
)

const prUsageText = "wtp pr <number> [--remote origin] [--branch name] [--quiet]"

// NewPrCommand creates the pr command definition
func NewPrCommand() *cli.Command {
	return &cli.Command{
		Name:      "pr",
		Usage:     "Create a worktree from a pull/merge request",
		UsageText: prUsageText,
		Description: "Fetch a pull request (GitHub) or merge request (GitLab) and create a worktree for it.\n\n" +
			"The forge type is auto-detected from the remote URL.\n\n" +
			"Examples:\n" +
			"  wtp pr 123                      Fetch PR #123 from origin\n" +
			"  wtp pr 456 --remote upstream     Fetch from upstream remote\n" +
			"  wtp pr 789 -b review-fix         Use custom branch name",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "remote",
				Aliases: []string{"r"},
				Value:   "origin",
				Usage:   "Remote name to fetch from",
			},
			&cli.StringFlag{
				Name:    "branch",
				Aliases: []string{"b"},
				Usage:   "Custom local branch name (default: pr-<number>)",
			},
			&cli.BoolFlag{
				Name:    "quiet",
				Aliases: []string{"q"},
				Usage:   "Output only worktree path to stdout",
			},
		},
		Action: prCommand,
	}
}

func prCommand(_ context.Context, cmd *cli.Command) error {
	stdoutWriter, statusWriter := resolveAddWriters(cmd)
	stdoutWriter = wtpio.NewFlushingWriter(stdoutWriter)
	statusWriter = wtpio.NewFlushingWriter(statusWriter)

	if cmd.Args().Len() != 1 {
		return errors.PRNumberRequired()
	}

	prNumber := cmd.Args().Get(0)
	if n, err := strconv.Atoi(prNumber); err != nil || n <= 0 {
		return errors.InvalidPRNumber(prNumber)
	}

	repo, cfg, mainRepoPath, err := setupRepoAndConfig()
	if err != nil {
		return err
	}

	branchName := cmd.String("branch")
	if branchName == "" {
		branchName = "pr-" + prNumber
	}

	remote := cmd.String("remote")
	fetchRef, err := buildFetchRef(repo, remote, prNumber)
	if err != nil {
		return err
	}

	executor := command.NewRealExecutor()

	if fetchErr := fetchPR(executor, remote, fetchRef, branchName, prNumber); fetchErr != nil {
		return fetchErr
	}

	workTreePath, err := createPRWorktree(executor, cfg, mainRepoPath, branchName)
	if err != nil {
		return err
	}

	if err := executePostCreateHooks(statusWriter, cfg, mainRepoPath, workTreePath); err != nil {
		if _, warnErr := fmt.Fprintf(statusWriter, "Warning: Hook execution failed: %v\n", err); warnErr != nil {
			return warnErr
		}
	}

	return outputPRResult(stdoutWriter, cmd, branchName, workTreePath, cfg, mainRepoPath)
}

func fetchPR(executor command.Executor, remote, fetchRef, branchName, prNumber string) error {
	fetchCmd := command.Command{
		Name: "git",
		Args: []string{"fetch", remote, fmt.Sprintf("%s:%s", fetchRef, branchName)},
	}
	result, err := executor.Execute([]command.Command{fetchCmd})
	if err != nil {
		return err
	}
	if len(result.Results) > 0 && result.Results[0].Error != nil {
		return fmt.Errorf("failed to fetch PR #%s: %s",
			prNumber, result.Results[0].Output)
	}
	return nil
}

func createPRWorktree(
	executor command.Executor,
	cfg *config.Config,
	mainRepoPath, branchName string,
) (string, error) {
	workTreePath := cfg.ResolveWorktreePath(mainRepoPath, branchName)
	worktreeCmd := command.GitWorktreeAdd(workTreePath, branchName, command.GitWorktreeAddOptions{})
	result, err := executor.Execute([]command.Command{worktreeCmd})
	if err != nil {
		return "", err
	}
	if len(result.Results) > 0 && result.Results[0].Error != nil {
		return "", analyzeGitWorktreeError(workTreePath, branchName,
			result.Results[0].Error, result.Results[0].Output)
	}
	return workTreePath, nil
}

func outputPRResult(
	w io.Writer,
	cmd *cli.Command,
	branchName, workTreePath string,
	cfg *config.Config,
	mainRepoPath string,
) error {
	if cmd.Bool("quiet") {
		_, err := fmt.Fprintln(w, workTreePath)
		return err
	}
	return displaySuccessMessage(w, branchName, workTreePath, cfg, mainRepoPath)
}

type forgeType int

const (
	forgeGitHub forgeType = iota
	forgeGitLab
)

func detectForgeType(repo *git.Repository, remote string) (forgeType, error) {
	url, err := repo.GetRemoteURL(remote)
	if err != nil {
		return 0, fmt.Errorf("failed to get URL for remote '%s': %w", remote, err)
	}
	if strings.Contains(strings.ToLower(url), "gitlab") {
		return forgeGitLab, nil
	}
	return forgeGitHub, nil
}

func buildFetchRef(repo *git.Repository, remote, prNumber string) (string, error) {
	forge, err := detectForgeType(repo, remote)
	if err != nil {
		return "", err
	}
	switch forge {
	case forgeGitHub:
		return fmt.Sprintf("refs/pull/%s/head", prNumber), nil
	case forgeGitLab:
		return fmt.Sprintf("refs/merge-requests/%s/head", prNumber), nil
	}
	return fmt.Sprintf("refs/pull/%s/head", prNumber), nil
}
