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
    gitconfig "gopkg.in/src-d/go-git.v4/config"
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

func openRepo(path string, bare bool) (*git.Repository, error) {
    var err error
    var repo *git.Repository
    // Create the source dir, bare
    repo, err = git.PlainInit(path, bare)
    if err == git.ErrRepositoryAlreadyExists {
        repo, err = git.PlainOpen(path)
    }

    return repo, err
}

func getRemoteBranch(r *git.Repository, name string, url string) (*git.Remote, error) {
    var err error
    var remote *git.Remote

    remote, err = r.Remote(name)
    if err == nil {

    }

    return remote, err
}

func initSourceRepo(bgctx context.Context, cfg *Config) (*git.Repository, error) {
    var err error
    var repo *git.Repository
    var refspec string

    ctx, cancelContext := context.WithCancel(bgctx)
    defer cancelContext()

    // open the source dir
    path := filepath.Join(cfg.WorkDir, cfg.SourceDir)
    fmt.Println("Opening source repo: ", path)

    repo, err = openRepo(path, true)
    if err != nil {
        goto out
    }

    // Create branch if it doesn't exist
    _, err = repo.CreateRemote(&gitconfig.RemoteConfig{
        Name: "origin",
        URLs: []string{cfg.SourceRepo},
    })
    if err != nil && err != git.ErrRemoteExists {
        goto out
    }

    refspec = fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s",
        cfg.SourceBranch, "origin", cfg.SourceBranch)


    fmt.Println("Synchronizing refs from remote: ", refspec)
    err = repo.FetchContext(ctx, &git.FetchOptions{
        RemoteName: "origin",
        RefSpecs:   []gitconfig.RefSpec{gitconfig.RefSpec(refspec)},
        Progress:   os.Stdout,
        Force:      true,
    })

out:
    return repo, err
}

func openStage(ctx context.Context, cfg *Config) (*git.Repository, error) {
    var err error
    var repo *git.Repository

    // open the source dir
    path := filepath.Join(cfg.WorkDir, cfg.StageDir)
    fmt.Println("Opening stage repo: ", path)

    // Create the source dir, bare
    repo, err = openRepo(path, true)

    return repo, err
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
    sourceBranch := flag.String("source-branch", "master", "Source git branch (default: master)")
    targetRepo := flag.String("target-repo", "", "Target git repository (required)")
    targetBranch := flag.String("target-branch", "master", "Target git branch (default: master)")
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
                signal.Stop(sigs)
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
    var ret int
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
    _, err = initSourceRepo(ctx, &cfg)
    if err != nil {
        if err == git.NoErrAlreadyUpToDate {
            fmt.Println(err)
            ret = 0
            goto out
        }
        fmt.Fprintf(os.Stderr, "Could not open source repository: %s\n", err)
        ret = 1
        goto out
    }

    _, err = openStage(ctx, &cfg)
    if err != nil {
        fmt.Fprintf(os.Stderr, "Could not open stage directory: %s\n", err)
        ret = 1
        goto out
    }

    // Determine starting point

out:
    // finished work.
    cancelContexts()

    // wait for all work to finish
    wg.Wait()

    os.Exit(ret)
}
