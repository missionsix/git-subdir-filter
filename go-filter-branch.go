package main

import (
    "context"
    "errors"
    "flag"
    "fmt"
    //    "log"
    //    "net"
    "github.com/caarlos0/env"
    "os"
    "os/signal"
    "sync"
    "syscall"
    //    "gopkg.in/src-d/go-git.v4"
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

func setupEnvironment(cfg *Config) error {

    srepo := flag.String("source-repo", "", "Source git repository (required)")
    trepo := flag.String("target-repo", "", "Target git repository (required)")
    sbranch := flag.String("source-branch", "", "Source git branch (required)")
    tbranch := flag.String("target-branch", "", "Target git branch (required)")
    fdir := flag.String("filter-dir", "", "Filter Directory (required)")

    // Parse environment
    err := env.Parse(cfg)
    if err != nil {
        return err
    }

    // flags override environment
    flag.Parse()

    // verify required flags
    if *srepo == "" {
        return errors.New("--source-repo is required")
    }
    if *trepo == "" {
        return errors.New("--target-repo is required")
    }
    if *sbranch == "" {
        return errors.New("--source-branch is required")
    }
    if *tbranch == "" {
        return errors.New("--target-branch is required")
    }
    if *fdir == "" {
        return errors.New("--filter-dir is required")
    }

    cfg.SourceRepo = srepo
    cfg.SourceRepo = srepo

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

func main() {
    var wg sync.WaitGroup

    cfg := Config{}

    err := setupEnvironment(&cfg)
    if err != nil {
        usage(err)
    }

    // Setup runtime context
    ctx, cancelContexts := context.WithCancel(context.Background())
    SignalHandler(ctx, cancelContexts, &wg)

    // finished work.
    cancelContexts()

    // wait for all work to finish
    wg.Wait()
}
