// git-subdir-filter extracts a subdirectory's history from a source
// repository branch and force-pushes it to a target repository branch.
//
// Example:
//
//	git-subdir-filter -source-repo=git@github.com:org/mono.git \
//	    -filter-dir=lib/foo -target-repo=git@github.com:org/foo.git
//
// MIT License, Copyright (c) 2018 Patrick Andrew. See LICENSE.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	git "github.com/libgit2/git2go/v34"
	"github.com/missionsix/git-subdir-filter/internal/config"
	"github.com/missionsix/git-subdir-filter/internal/remote"
	"github.com/missionsix/git-subdir-filter/internal/split"
)

func main() {
	os.Exit(run())
}

func run() int {
	cfg, err := config.Load(os.Args[1:], os.Getenv, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	context.AfterFunc(ctx, stop) // a second signal kills the process

	if err := mirror(ctx, cfg, log); err != nil {
		log.Error("failed", "err", err)
		return 1
	}
	return 0
}

// mirror fetches the source branch, rewrites it, and pushes the result.
func mirror(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	log.Info("starting", "work_dir", cfg.WorkDir, "source", cfg.SourceRepo, "source_branch", cfg.SourceBranch,
		"target", cfg.TargetRepo, "target_branch", cfg.TargetBranch, "filter_dir", cfg.FilterDir)

	if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}
	repo, err := openOrInit(cfg.WorkDir)
	if err != nil {
		return err
	}
	defer repo.Free()

	// Refs are namespaced per source/branch/prefix so jobs can share a work dir.
	mapPath, ns := split.StateLocations(cfg.WorkDir, cfg.SourceRepo, cfg.SourceBranch, cfg.FilterDir)
	tracking, workRef := ns+"source", ns+"head"

	fetchSpec := fmt.Sprintf("+refs/heads/%s:%s", cfg.SourceBranch, tracking)
	log.Info("fetching", "refspec", fetchSpec)
	if err := remote.Fetch(ctx, repo, cfg.SourceRepo, fetchSpec, auth(cfg)); err != nil {
		return fmt.Errorf("fetch %s: %w", cfg.SourceRepo, err)
	}
	tip, err := resolve(repo, tracking)
	if err != nil {
		return err
	}

	s := &split.Splitter{Repo: repo, Prefix: cfg.FilterDir, MapPath: mapPath, StateRef: ns + "source-tip", Log: log}
	res, err := s.Split(ctx, tip)
	if err != nil {
		return err
	}
	if res.Head == nil {
		return fmt.Errorf("%q does not exist in %s", cfg.FilterDir, cfg.SourceBranch)
	}

	ref, err := repo.References.Create(workRef, res.Head, true, "subdir-filter")
	if err != nil {
		return fmt.Errorf("update %s: %w", workRef, err)
	}
	ref.Free()

	pushSpec := fmt.Sprintf("+%s:refs/heads/%s", workRef, cfg.TargetBranch)
	log.Info("pushing", "head", res.Head.String(), "refspec", pushSpec)
	if err := remote.Push(ctx, repo, cfg.TargetRepo, pushSpec, auth(cfg)); err != nil {
		return fmt.Errorf("push %s: %w", cfg.TargetRepo, err)
	}
	return nil
}

// auth resolves SSH settings; the passphrase comes only from SSH_PASSKEY
// so it never appears in the process list.
func auth(cfg *config.Config) remote.Auth {
	home, _ := os.UserHomeDir()
	a := remote.Auth{
		KeyPath:               cfg.SSHKey,
		Passphrase:            os.Getenv("SSH_PASSKEY"),
		UseAgent:              true,
		KnownHosts:            []string{cfg.KnownHosts},
		InsecureSkipHostCheck: cfg.InsecureSkipHostCheck,
	}
	if a.KeyPath == "" {
		a.KeyPath = remote.DefaultKeyPath(home)
	}
	if cfg.KnownHosts == "" {
		a.KnownHosts = []string{filepath.Join(home, ".ssh", "known_hosts")}
	}
	return a
}

// openOrInit opens the work repository, creating a bare one only in an
// empty directory so an unrelated directory is never written into.
func openOrInit(path string) (*git.Repository, error) {
	repo, err := git.OpenRepositoryExtended(path, git.RepositoryOpenNoSearch, "")
	if err == nil {
		return repo, nil
	}
	if !git.IsErrorCode(err, git.ErrorCodeNotFound) {
		return nil, fmt.Errorf("open work dir %s: %w", path, err)
	}
	entries, rerr := os.ReadDir(path)
	if rerr != nil {
		return nil, rerr
	}
	if len(entries) > 0 {
		return nil, fmt.Errorf("work dir %s is not a git repository and is not empty", path)
	}
	return git.InitRepository(path, true)
}

func resolve(repo *git.Repository, name string) (*git.Oid, error) {
	ref, err := repo.References.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("lookup %s: %w", name, err)
	}
	defer ref.Free()
	return ref.Target(), nil
}
