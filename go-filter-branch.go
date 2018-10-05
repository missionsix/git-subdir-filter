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
    gitplumb "gopkg.in/src-d/go-git.v4/plumbing"
    gitobj "gopkg.in/src-d/go-git.v4/plumbing/object"
    "os"
    "os/signal"
    "path/filepath"
    "sync"
    "syscall"
)

const (
    GIT_ORIGIN = "origin"
    // queue depth between stages
    STAGE_CHAN_DEPTH = 20
    SOURCE_WORK_BRANCH = "incoming"
    STAGE_WORK_BRANCH = "staging"
    RUNTIME_CONFIG = "runtime_config"
    RUNTIME_SOURCE_REPO = "runtime_source"
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


func readInputRevisions(ctx context.Context, wg *sync.WaitGroup,
    inputChan chan<- gitplumb.Hash) {

    wg.Add(1)
    go func() {
        var err error
        var q []gitplumb.Hash
        var ref *gitplumb.Reference
        var c *gitobj.Commit

        defer wg.Done()
        defer close(inputChan)

        cfg := ctx.Value(RUNTIME_CONFIG).(*Config)
        source := ctx.Value(RUNTIME_SOURCE_REPO).(*git.Repository)

        // Get reference
        refname := gitplumb.ReferenceName(
            fmt.Sprintf("refs/remotes/%s/%s", GIT_ORIGIN, cfg.SourceBranch))
        ref, err = source.Reference(refname, false)

        c, err = source.CommitObject(ref.Hash())
        if err != nil {
            fmt.Fprintf(os.Stderr, "Fatal: %s\n", ref.Hash(), err)
            return
        }
        iter := gitobj.NewCommitPostorderIter(c, nil)

        var i int
        fmt.Println("Building commit vector...")
        // push commits into vec
        _ = iter.ForEach(func(c *gitobj.Commit) error {
            q = append(q, c.Hash)
//          fmt.Println("\t", c.Hash)
            if i == 10 {
                return errors.New("ErrStop")
            }
            i += 1
            return nil
        })

        fmt.Println("Sending in-order commits to pipeline")
        // pop back commits into channel
        var h gitplumb.Hash
        for len(q) > 0 {
            h, q = q[len(q)-1], q[:len(q)-1]
            select {
            case <-ctx.Done():
                return
            case inputChan <- h:
                continue
            }
        }
        fmt.Println("Done with sends")
    }()
}

func processRevisions(ctx context.Context, wg *sync.WaitGroup,
    inputChan <-chan gitplumb.Hash) {

    // Create a base tree for initial DIFF
    root := gitobj.Tree {
        Hash: gitplumb.ZeroHash,
    }
    var prevTree *gitobj.Tree = &root

    wg.Add(1)
    go func() {
        defer wg.Done()

        // cfg := ctx.Value(RUNTIME_CONFIG).(Config)
        source := ctx.Value(RUNTIME_SOURCE_REPO).(*git.Repository)


        dumpCommit := func(h gitplumb.Hash) {
            // filter hash
            c, _ := source.CommitObject(h)

            t, err := c.Tree()
            if err != nil {
                return
            }

            fmt.Println(t.Hash)

            patch, err := prevTree.Patch(t)

            fmt.Println("Got patch")

            for _,fp := range patch.FilePatches() {
                _, to := fp.Files()
                if to != nil {
                   fmt.Println(to.Path())
                }
            }
            prevTree = t

/*
            fileIter, err := c.Files()
            if err != nil {
                fmt.Fprintf(os.Stderr, "Fatal: %s", err)
                cancel()
                return
            }
            
            fmt.Println("Commit Contents:")
//          l := len(cfg.FilterDirectory)
            _ = fileIter.ForEach(func(f *gitobj.File) error {
                fmt.Println(c.ID(), " ", f.Name)

                if f.Name[:l] != cfg.FilterDirectory {
                    return nil
                }
                fmt.Println(c.ID(), " ", f.Name)
                //return filtIter.StopIter


                return nil

 })
*/
        }

        for {
            select {
            case <-ctx.Done():
                return
            case h, ok := <-inputChan:
                if !ok {
                    return
                }
                dumpCommit(h)
            }
        }
    }()
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

func makeRefSpec(remote string, branch string) string {
    return fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s",
        branch, remote, branch)
}

func initSourceRepo(bgctx context.Context) (*git.Repository, error) {
    var err error
    var repo *git.Repository
    var refspec string

    ctx, cancelContext := context.WithCancel(bgctx)
    defer cancelContext()

    cfg := ctx.Value(RUNTIME_CONFIG).(*Config)

    // open the source dir
    path := filepath.Join(cfg.WorkDir, cfg.SourceDir)
    fmt.Println("Opening source repo: ", path)

    repo, err = openRepo(path, true)
    if err != nil {
        goto out
    }

    // Create remote branch if it doesn't exist
    _, err = repo.CreateRemote(&gitconfig.RemoteConfig{
        Name: GIT_ORIGIN,
        URLs: []string{cfg.SourceRepo},
    })
    if err != nil && err != git.ErrRemoteExists {
        goto out
    }

    refspec = makeRefSpec(GIT_ORIGIN, cfg.SourceBranch)

    fmt.Println("Synchronizing refs from remote: ", refspec)
    err = repo.FetchContext(ctx, &git.FetchOptions{
        RemoteName: GIT_ORIGIN,
        RefSpecs:   []gitconfig.RefSpec{gitconfig.RefSpec(refspec)},
        Progress:   nil,
        Force:      true,
    })

out:

    return repo, err
}

func openStage(ctx context.Context) (*git.Repository, error) {
    var err error
    var repo *git.Repository

    cfg := ctx.Value(RUNTIME_CONFIG).(*Config)

    // open the source dir
    path := filepath.Join(cfg.WorkDir, cfg.StageDir)
    fmt.Println("Opening stage repo: ", path)

    // Create and open the stage dir, bare
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
        return errors.New("-source-repo is required")
    }
    if *targetRepo == "" {
        return errors.New("-target-repo is required")
    }
    if *filterDir == "" {
        return errors.New("-filter-dir is required")
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

// Global signal handler, cancels the context
// return cancel func for graceful teardown
func SignalHandler(ctx context.Context) context.CancelFunc {
    sigs := make(chan os.Signal, 1)
    signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

    c, cancel := context.WithCancel(ctx)

    go func() {
        for {
            select {
            case <-c.Done():
                signal.Stop(sigs)
                fmt.Println("Exiting signal handler")
                return
            case s := <-sigs:
                fmt.Println("Received Signal: ", s)
                cancel()
            }
        }
    }()

    return cancel
}

func main() {
    var ret int
    var err error
    var wg sync.WaitGroup
    var inputChan chan gitplumb.Hash
    
    cfg := Config{}

    err = setupEnvironment(&cfg)
    if err != nil {
        usage(err)
    }

    err = buildEnvironment(&cfg)
    if err != nil {
        fmt.Fprintf(os.Stderr, "Failed to create working directory: %s\n", err)
        os.Exit(1)
    }

    // Setup runtime context
    ctx := context.Background()
    cancel := SignalHandler(ctx)

    ctx = context.WithValue(ctx, RUNTIME_CONFIG, &cfg)

    // Clone and sync source repo
    var source *git.Repository
    source, err = initSourceRepo(ctx)
    if err != nil && err != git.NoErrAlreadyUpToDate {
        fmt.Fprintf(os.Stderr, "Failed to create source repository: %s\n", err)
        ret = 1
        goto out
    }
    err = nil
    ctx = context.WithValue(ctx, RUNTIME_SOURCE_REPO, source)

    _, err = openStage(ctx)
    if err != nil {
        fmt.Fprintf(os.Stderr, "Failed to create stage directory: %s\n", err)
        ret = 1
        goto out
    }
//     ctx = context.WithValue(ctx, RUNTIME_SOURCE_REPO, source)

    // Determine starting point

    // hand off channel (mvar)
    inputChan = make(chan gitplumb.Hash, 1000)

    // Launch sequency input processing
    readInputRevisions(ctx, &wg, inputChan)

    // Start workers
    processRevisions(ctx, &wg, inputChan)

    // filter channel routine
//  filteredChan := make(chan gitobj.Commit, 1000)  


    // contains new commits
    //filteredChan := make(chan gitobj.Commit, 1000)
/*  
    // write to tree
    wg.Add(1)
    go func() {
        defer wg.Done()
        for {
            select {
            case <- ctx.Done:
                return
            }
        }
    }()
 */

    // wait for all work to finish
    wg.Wait()

out:

    cancel()

    os.Exit(ret)
}
