package main

import (
    "context"
    "errors"
    "flag"
    "fmt"
    //    "log"
    //    "net"
    "github.com/caarlos0/env"
    "gopkg.in/src-d/go-git.v4"
    "os"
    "os/signal"
    "path/filepath"
    "sync"
    "syscall"
)

type Config struct {
    WorkDir         string `env:"FILTER_DIR" envDefault:"${PWD}/work" envExpand:"true"`
    SourceDir       string
    StageDir        string
    TargetDir       string
    SourceRepo      string `env:"SOURCE_REPO"`
    SourceBranch    string `env:"SOURCE_BRANCH"`
    TargetRepo      string `env:"SOURCE_REPO"`
    TargetBranch    string `env:"TARGET_BRANCH"`
    FilterDirectory string
}

func openSourceRepo(ctx context.Context, cfg *Config) (*git.Worktree, error) {
    var repo *git.Repository
    var w *git.Worktree
    var err error

    // open the source dir
    path := filepath.Join(cfg.WorkDir, cfg.SourceDir)
    fmt.Println("Opening git repo", path)

    // verify existence of git directory
    if _, err := os.Stat(filepath.join(path, ".git")); os.IsNotExist(err) {
        git.Init()
    }

    repo, err = git.PlainOpen(path)
    if err != nil {
        // check error, init if doesn't exist
        goto out
    }

    w, err = repo.Worktree()
    if err != nil {
        return nil, err
    }

    // sync with remote
    err = w.Pull(&git.PullOptions{RemoteName: "origin"})

out:

    return w, err
}

func openStage(ctx context.Context, cfg *Config) (*git.Worktree, error) {
    return nil, nil
}

func ensureDir(path string) error {
    err := os.MkdirAll(path, os.ModePerm)
    if err != nil {
        return err
    }
    return nil
}

// buildEnvironment creates the set work working directories
func buildEnvironment(cfg *Config) error {
    var err error
    var dirs [4]string

    dirs[0] = cfg.WorkDir
    dirs[1] = filepath.Join(cfg.WorkDir, cfg.SourceDir)
    dirs[2] = filepath.Join(cfg.WorkDir, cfg.TargetDir)
    dirs[3] = filepath.Join(cfg.WorkDir, cfg.StageDir)

    for i := 0; i < 4; i++ {
        err = ensureDir(dirs[i])
        if err != nil {
            return err
        }
    }

    return nil
}

// setupEnvironment sets the runtime
func setupEnvironment(cfg *Config) error {

    sourceRepo := flag.String("source-repo", "", "Source git repository (required)")
    targetRepo := flag.String("target-repo", "", "Target git repository (required)")
    sourceBranch := flag.String("source-branch", "", "Source git branch (required)")
    targetBranch := flag.String("target-branch", "", "Target git branch (required)")
    filterDir := flag.String("filter-dir", "", "Filter Directory (required)")

    // Parse environment
    err := env.Parse(cfg)
    if err != nil {
        return err
    }

    // flags override environment
    flag.Parse()

    // verify required flags
    if *sourceRepo == "" {
        return errors.New("--source-repo is required")
    }
    if *targetRepo == "" {
        return errors.New("--target-repo is required")
    }
    if *sourceBranch == "" {
        return errors.New("--source-branch is required")
    }
    if *targetBranch == "" {
        return errors.New("--target-branch is required")
    }
    if *filterDir == "" {
        return errors.New("--filter-dir is required")
    }

    cfg.SourceRepo = *sourceRepo
    cfg.TargetRepo = *targetRepo
    cfg.SourceBranch = *sourceBranch
    cfg.TargetBranch = *targetBranch
    cfg.FilterDirectory = *filterDir

    // set paths
    if cfg.SourceDir == "" {
        cfg.SourceDir = "source"
    }
    if cfg.TargetDir == "" {
        cfg.TargetDir = "target"
    }
    if cfg.StageDir == "" {
        cfg.StageDir = "stage"
    }

    fmt.Println("Working Directory: ", cfg.WorkDir)

    fmt.Println("Source Repository: ", cfg.SourceRepo)
    fmt.Println("Source Branch: ", cfg.SourceBranch)
    fmt.Println("Target Repository: ", cfg.TargetRepo)
    fmt.Println("Target Branch: ", cfg.TargetBranch)
    fmt.Println("Source Tree: ", cfg.SourceDir)
    fmt.Println("Target Tree: ", cfg.TargetDir)
    fmt.Println("Stage Tree: ", cfg.StageDir)

    return nil
}

func usage(err error) {
    fmt.Fprintf(os.Stderr, "%+v\n", err)

    flag.PrintDefaults()

    os.Exit(1)
}

func SignalHandler(ctx context.Context, cancel context.CancelFunc, wg *sync.WaitGroup) {
    sigs := make(chan os.Signal, 1)
    signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

    wg.Add(1)
    go func() {
        defer wg.Done()
        for {
            select {
            case <-ctx.Done():
                fmt.Println("Exiting signal handler")
                return
            case s := <-sigs:
                fmt.Println("Received Signal: ", s)
                cancel()
            }
        }
    }()
}

func main() {
    var err error
    var wg sync.WaitGroup

    cfg := Config{}

    err = setupEnvironment(&cfg)
    if err != nil {
        usage(err)
    }

    err = buildEnvironment(&cfg)
    if err != nil {
        fmt.Fprintf(os.Stderr, "Could not create working directory: %s\n", err)
        os.Exit(1)
    }

    // Setup runtime context
    ctx, cancelContexts := context.WithCancel(context.Background())
    SignalHandler(ctx, cancelContexts, &wg)

    // Clone snd sync source repo
    _, err = openSourceRepo(ctx, &cfg)
    if err != nil {
        fmt.Fprintf(os.Stderr, "Could not open source repository: %s\n", err)
        os.Exit(1)
    }
    _, err = openStage(ctx, &cfg)
    if err != nil {
        fmt.Fprintf(os.Stderr, "Could not open stage directory: %s\n", err)
        os.Exit(1)
    }

    // Determine starting point

    // finished work.
    cancelContexts()

    // wait for all work to finish
    wg.Wait()
}
