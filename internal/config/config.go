// Package config loads git-subdir-filter settings from flags and the environment.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path"
	"runtime"
	"strconv"
	"strings"
)

// Config holds the resolved runtime settings.
type Config struct {
	WorkDir      string
	SourceRepo   string
	SourceBranch string
	TargetRepo   string
	TargetBranch string
	FilterDir    string
	Workers      int
}

// Load parses args (without the program name). Each flag defaults to its
// environment variable when set, so precedence is flag > env > built-in default.
//
// Example: with TARGET_REPO=git@a:x.git in the environment,
// Load([]string{"-target-repo=git@b:y.git", ...}, os.Getenv) yields git@b:y.git.
func Load(args []string, getenv func(string) string, out io.Writer) (*Config, error) {
	fs := flag.NewFlagSet("git-subdir-filter", flag.ContinueOnError)
	fs.SetOutput(out)

	envOr := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}

	var c Config
	fs.StringVar(&c.WorkDir, "work-dir", envOr("WORKDIR", "work"), "working repository directory (env WORKDIR)")
	fs.StringVar(&c.SourceRepo, "source-repo", envOr("SOURCE_REPO", ""), "source repository URL, required (env SOURCE_REPO)")
	fs.StringVar(&c.SourceBranch, "source-branch", envOr("SOURCE_BRANCH", "master"), "source branch (env SOURCE_BRANCH)")
	fs.StringVar(&c.TargetRepo, "target-repo", envOr("TARGET_REPO", ""), "target repository URL, required (env TARGET_REPO)")
	fs.StringVar(&c.TargetBranch, "target-branch", envOr("TARGET_BRANCH", "master"), "target branch (env TARGET_BRANCH)")
	fs.StringVar(&c.FilterDir, "filter-dir", envOr("FILTER_DIR", ""), "subdirectory to extract, required (env FILTER_DIR)")

	workers, err := strconv.Atoi(envOr("WORKERS", "0"))
	if err != nil {
		return nil, fmt.Errorf("WORKERS: %w", err)
	}
	fs.IntVar(&c.Workers, "workers", workers, "worker count, 0 = CPU count (env WORKERS)")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if c.Workers <= 0 {
		c.Workers = runtime.NumCPU()
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	var errs []error
	for _, r := range []struct{ name, val string }{
		{"-source-repo", c.SourceRepo},
		{"-target-repo", c.TargetRepo},
		{"-filter-dir", c.FilterDir},
		{"-source-branch", c.SourceBranch},
		{"-target-branch", c.TargetBranch},
		{"-work-dir", c.WorkDir},
	} {
		if r.val == "" {
			errs = append(errs, fmt.Errorf("%s is required", r.name))
		}
	}
	if c.FilterDir != "" {
		dir, err := CleanPrefix(c.FilterDir)
		if err != nil {
			errs = append(errs, err)
		}
		c.FilterDir = dir
	}
	return errors.Join(errs...)
}

// CleanPrefix normalizes a repository-relative directory, for example
// "/lib//foo/" -> "lib/foo". It rejects the root and paths escaping it.
func CleanPrefix(p string) (string, error) {
	c := strings.Trim(path.Clean("/"+p), "/")
	if c == "" || strings.HasPrefix(path.Clean(p), "..") {
		return "", fmt.Errorf("-filter-dir %q must name a subdirectory", p)
	}
	return c, nil
}
