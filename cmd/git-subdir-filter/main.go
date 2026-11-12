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
	"syscall"

	git "github.com/libgit2/git2go/v34"
	"github.com/missionsix/git-subdir-filter/internal/config"
	"github.com/missionsix/git-subdir-filter/internal/split"
)

const (
	sourceRemote = "origin"
	targetRemote = "target"
	// workBranch holds the rewritten history inside the work repository.
	workBranch = "refs/heads/incoming"
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

	tracking := fmt.Sprintf("refs/remotes/%s/%s", sourceRemote, cfg.SourceBranch)
	fetchSpec := fmt.Sprintf("+refs/heads/%s:%s", cfg.SourceBranch, tracking)
	log.Info("fetching", "refspec", fetchSpec)
	if err := fetch(repo, cfg.SourceRepo, fetchSpec); err != nil {
		return fmt.Errorf("fetch %s: %w", cfg.SourceRepo, err)
	}
	tip, err := resolve(repo, tracking)
	if err != nil {
		return err
	}

	mapPath, stateRef := split.StateLocations(cfg.WorkDir, cfg.FilterDir)
	s := &split.Splitter{Repo: repo, Prefix: cfg.FilterDir, MapPath: mapPath, StateRef: stateRef, Log: log}
	res, err := s.Split(ctx, tip)
	if err != nil {
		return err
	}
	if res.Head == nil {
		return fmt.Errorf("%q does not exist in %s", cfg.FilterDir, cfg.SourceBranch)
	}

	ref, err := repo.References.Create(workBranch, res.Head, true, "subdir-filter")
	if err != nil {
		return fmt.Errorf("update %s: %w", workBranch, err)
	}
	ref.Free()

	pushSpec := fmt.Sprintf("+%s:refs/heads/%s", workBranch, cfg.TargetBranch)
	log.Info("pushing", "head", res.Head.String(), "refspec", pushSpec)
	if err := push(repo, cfg.TargetRepo, pushSpec); err != nil {
		return fmt.Errorf("push %s: %w", cfg.TargetRepo, err)
	}
	return nil
}

func openOrInit(path string) (*git.Repository, error) {
	repo, err := git.OpenRepository(path)
	if err == nil {
		return repo, nil
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

// remote returns the named remote, creating it or updating its URL.
func remote(repo *git.Repository, name, url string) (*git.Remote, error) {
	if _, err := repo.Remotes.Lookup(name); err != nil {
		return repo.Remotes.Create(name, url)
	}
	if err := repo.Remotes.SetUrl(name, url); err != nil {
		return nil, err
	}
	return repo.Remotes.Lookup(name)
}

func fetch(repo *git.Repository, url, refspec string) error {
	r, err := remote(repo, sourceRemote, url)
	if err != nil {
		return err
	}
	defer r.Free()
	return r.Fetch([]string{refspec}, &git.FetchOptions{
		DownloadTags:    git.DownloadTagsNone,
		RemoteCallbacks: callbacks(),
	}, "")
}

func push(repo *git.Repository, url, refspec string) error {
	r, err := remote(repo, targetRemote, url)
	if err != nil {
		return err
	}
	defer r.Free()
	return r.Push([]string{refspec}, &git.PushOptions{RemoteCallbacks: callbacks()})
}

// callbacks keeps the original ~/.ssh/id_rsa auth; replaced by internal/remote.
func callbacks() git.RemoteCallbacks {
	return git.RemoteCallbacks{
		CredentialsCallback: func(url, user string, _ git.CredentialType) (*git.Credential, error) {
			home := os.Getenv("HOME")
			return git.NewCredentialSSHKey(user, home+"/.ssh/id_rsa.pub", home+"/.ssh/id_rsa", os.Getenv("SSH_PASSKEY"))
		},
		CertificateCheckCallback: func(*git.Certificate, bool, string) error { return nil },
	}
}
