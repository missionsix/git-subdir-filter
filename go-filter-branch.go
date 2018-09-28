package main

import (
    "context"
    //"flag"
    "fmt"
    //    "log"
    //    "net"
    "os"
    "os/signal"
    "sync"
    "syscall"

    "github.com/caarlos0/env"
)

type Config struct {
    WorkDir      string `env:"FILTER_DIR" envDefault:"${PWD}/work" envExpand:"true"`
    SourceDir    string `env:"SOURCE_DIR" envDefault:"${PWD}/work/source" envExpand:"true"`
    SourceRepo   string `env:"SOURCE_REPO"`
    SourceBranch string `env:"SOURCE_BRANCH"`
    TargetRepo   string `env:"SOURCE_REPO"`
    TargetBranch string `env:"TARGET_BRANCH"`
    TargetDir    string `env:"TARGET_DIR" envDefault:"${PWD}/target/source" envExpand:"true"`
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
    var wg sync.WaitGroup

    cfg := Config{}

    //flag.Parse()
    err := env.Parse(&cfg)
    if err != nil {
        fmt.Fprintf(os.Stderr, "%+v\n", err)
    }

    // Setup runtime context
    ctx, cancelContexts := context.WithCancel(context.Background())
    SignalHandler(ctx, cancelContexts, &wg)

    fmt.Println("working...")

    // finished work.
    cancelContexts()

    // wait for all work to finish
    wg.Wait()
}
