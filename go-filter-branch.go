package main

import (
    "context"
    "errors"
    "flag"
    "fmt"
    //    "log"
    //    "net"
    "container/list"
    "github.com/caarlos0/env"
    "gopkg.in/libgit2/git2go.v26"
    "os"
    "os/signal"
    "path/filepath"
    "sync"
    "strings"
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
    RUNTIME_COMMIT_TREE = "commit_tree"
    RUNTIME_WORKERS = 1
)


type Config struct {
    WorkDir         string `env:"FILTER_DIR" envDefault:"${PWD}/work" envExpand:"true"`
    SourceDir       string `envDefault:"source"`
    StageDir        string `envDefault:"stage"`
    TargetDir       string `envDefault:"target"`
    SourceRepo      string `env:"SOURCE_REPO"`
    SourceBranch    string `env:"SOURCE_BRANCH"`
    TargetRepo      string `env:"SOURCE_REPO"`
    TargetBranch    string `env:"TARGET_BRANCH"`
    FilterDirectory string
}

type commitTree struct {
    List *list.List
//  Cond *sync.Cond
    Mtx sync.Mutex
}

type commitItem struct {
    ListElement *list.Element
    Commit *git.Commit
}

var matchCount int = 0

func rewriteWorker(ctx context.Context, wg *sync.WaitGroup, commits <-chan *commitItem) {
//  var err error
    var matchCount int = 0

    defer wg.Done()

    for {
        select {
        case <- ctx.Done():
            return
        case ci, ok := <-commits:
            if !ok {
                return
            }
            fmt.Printf("%d ", matchCount)
            fmt.Println(ci.Commit.Id())
            matchCount += 1
        }
    }
}

func rewriteCommits(ctx context.Context, wg *sync.WaitGroup, commits <-chan *commitItem) {
    workers := ctx.Value(RUNTIME_WORKERS).(int)

    fmt.Printf("Starting %d rewrite workers...\n", workers)
    wg.Add(workers)
    for i := 0; i < workers; i++ {
        go rewriteWorker(ctx, wg, commits)
    }
}

func matchTree(repo *git.Repository, tree *git.Tree, filter []string, n int) bool {
    var err error

    if len(filter) == n {
        matchCount += 1
        return true
    }

    te := tree.EntryByName(filter[n])
    if te == nil {
        return false
    }

    if te.Type != git.ObjectTree {
        return false
    }

    tree, err = repo.LookupTree(te.Id)
    if err != nil {
        return false
    }

    return matchTree(repo, tree, filter, n+1)
}

func filterWorker(ctx context.Context, wg *sync.WaitGroup,
    filterChan <-chan *commitItem, rewriteChan chan<- *commitItem) {
    var err error
    var tree *git.Tree

    defer wg.Done()
    defer close(rewriteChan)

    cfg := ctx.Value(RUNTIME_CONFIG).(*Config)
    commits := ctx.Value(RUNTIME_COMMIT_TREE).(*commitTree)
    repo := ctx.Value(RUNTIME_SOURCE_REPO).(*git.Repository)

    filterDirs := strings.Split(cfg.FilterDirectory, "/")

    for {
        select {
        case <- ctx.Done():
            return
        case ci, ok := <-filterChan:
            if !ok {
                return
            }
            tree, err = ci.Commit.Tree()
            if err != nil {
                continue
            }
            // if commit tree fails to match, drop it
            if ! matchTree(repo, tree, filterDirs, 0) {
                // remove element from list and garbage collect
//              commits.Mtx.Lock()
                commits.List.Remove(ci.ListElement)
//              commits.Mtx.Unlock()

                ci.ListElement = nil
            } else {
                // hand off to next worker
                rewriteChan <- ci
            }
        }
    }
}

func filterRevisions(ctx context.Context, wg *sync.WaitGroup,
    filterChan <-chan *commitItem, rewriteChan chan<- *commitItem) {

    fmt.Printf("Starting filter worker")
    wg.Add(1)
    go filterWorker(ctx, wg, filterChan, rewriteChan)
}

func stageRevisions(ctx context.Context, wg *sync.WaitGroup,
    inputChan <-chan *git.Commit, filterChan chan<- *commitItem) {

    commits := ctx.Value(RUNTIME_COMMIT_TREE).(*commitTree)

    wg.Add(1)
    go func() {
        defer wg.Done()
        defer close(filterChan)

        // Take lock for condition broadcast
        //commits.Mtx.Lock()

        input:
        for {
            select {
            case <-ctx.Done():
                return
            case c, ok := <-inputChan:
                if !ok {
                    break input
                }
                ci := &commitItem{
                    Commit: c,
                }

                // returns list-element (container)
                e := commits.List.PushFront(ci)
                ci.ListElement = e
            }
        }

        // Readers can savely observe items from filteredChan
        // and update the commits.List

        // Send ordered commits to be filtered
        for e := commits.List.Front(); e != nil; e = e.Next() {
            ci := e.Value.(*commitItem)
            filterChan <- ci
        }
        fmt.Println("Commits Staged")
    }()
}


func readInputRevisions(ctx context.Context, wg *sync.WaitGroup,
    inputChan chan<- *git.Commit) {

    wg.Add(1)
    go func() {
        var err error

        defer wg.Done()
        defer close(inputChan)

        cfg := ctx.Value(RUNTIME_CONFIG).(*Config)
        source := ctx.Value(RUNTIME_SOURCE_REPO).(*git.Repository)

        revwalk, err := source.Walk()
        if err != nil {
            return
        }
        defer revwalk.Free()

        // Set sort order - Topo reverse choronological
        revwalk.Sorting(git.SortTopological | git.SortTime)

        // Get reference
        refname := fmt.Sprintf("refs/remotes/%s/%s", GIT_ORIGIN, cfg.SourceBranch)
        revwalk.PushRef(refname)

        // Send commit revisions into input channel
        // in reverse chronological order
        revwalk.Iterate(func (c *git.Commit) bool {
            select {
            case <-ctx.Done():
                return false
            case inputChan <- c:
            }
            return true
        })
        fmt.Println("Reading inputs finished.")
    }()
}


// maekRefSpec generates a refspec string from remote and branch names
func makeRefSpec(remote string, branch string) string {
    return fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s",
        branch, remote, branch)
}

// FIXME
func credCheckCallback(url string, user string, allowedTypes git.CredType) (git.ErrorCode, *git.Cred) {
    ret, cred := git.NewCredSshKeyFromAgent(user)
    return git.ErrorCode(ret), &cred
}

// FIXME
func certCheckCallback(c *git.Certificate, valid bool, h string) git.ErrorCode {
    // need to validate host
    //if !valid {
      //  return git.ErrUser
    //}
    return git.ErrOk
}

// fetchRemote performs a blocking fetch from repository for the
// remote named GIT_ORIGIN from the config.SourceRepository URL
func fetchRemote(ctx context.Context, repo *git.Repository) error {
    var err error
    var refspec string
    var remote *git.Remote
    var options git.FetchOptions

    cfg := ctx.Value(RUNTIME_CONFIG).(*Config)

    remote, err = repo.Remotes.Lookup(GIT_ORIGIN)
    if err != nil {
        // Remote does not exist. Time to add:
        remote, err = repo.Remotes.Create(GIT_ORIGIN, cfg.SourceRepo)
        if err != nil {
            goto out
        }
    }

    refspec = makeRefSpec(GIT_ORIGIN, cfg.SourceBranch)

    options = git.FetchOptions{
        Prune: git.FetchNoPrune,
        DownloadTags: git.DownloadTagsAuto,
        RemoteCallbacks: git.RemoteCallbacks {
            CredentialsCallback: credCheckCallback,
            CertificateCheckCallback: certCheckCallback,
        },
    }

    // blocking
    err = remote.Fetch([]string{refspec}, &options, "")
    if err != nil {
        fmt.Println(err)
    }

out:
    return err
}

// openRepository will create (if it doesn't exist) and open the 
// source repository.
func openRepository(ctx context.Context, name string) (*git.Repository, error) {
    var err error
    var repo *git.Repository

    cfg := ctx.Value(RUNTIME_CONFIG).(*Config)

    // open the source dir
    path := filepath.Join(cfg.WorkDir, name)
    fmt.Println("Opening repo: ", path)

    // Create the source dir, bare
    repo, err = git.InitRepository(path, true)
    if err != nil {
        //repo, err = git.PlainOpen(path)
        fmt.Fprintf(os.Stderr, "%s\n", err)
        os.Exit(1)
    }

    return repo, err
}

// buildEnvironment creates the set of working directories
func buildWorkEnvironment(cfg *Config) error {
    var err error
    var dirs [4]string

    dirs[0] = cfg.WorkDir
    dirs[1] = filepath.Join(cfg.WorkDir, cfg.SourceDir)
    dirs[2] = filepath.Join(cfg.WorkDir, cfg.TargetDir)
    dirs[3] = filepath.Join(cfg.WorkDir, cfg.StageDir)

    for i := 0; i < 4; i++ {
        err = os.MkdirAll(dirs[i], os.ModePerm)
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
    var inputChan chan *git.Commit
    var filterChan chan *commitItem
    var rewriteChan chan *commitItem
    var commits *commitTree

    cfg := Config{}

    err = setupEnvironment(&cfg)
    if err != nil {
        usage(err)
    }

    err = buildWorkEnvironment(&cfg)
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
    source, err = openRepository(ctx, cfg.SourceDir)
    if err != nil {
        goto out
    }

    // Blocking fetch
    err = fetchRemote(ctx, source)
    if err != nil {
        goto out
    }

    _, err = openRepository(ctx, cfg.StageDir)
    if err != nil {
        goto out
    }

    // Create work structures
    commits = &commitTree {
        List: list.New(),
//      BTree: btree.New(BTREE_DEGREE),
    }

    ctx = context.WithValue(ctx, RUNTIME_SOURCE_REPO, source)
    ctx = context.WithValue(ctx, RUNTIME_COMMIT_TREE, commits)

    // Hand off channel (mvar)
    inputChan = make(chan *git.Commit, 1000)

    // Launch sequency input processing
    readInputRevisions(ctx, &wg, inputChan)

    filterChan = make(chan *commitItem, 1000)

    ctx = context.WithValue(ctx, RUNTIME_WORKERS, RUNTIME_WORKERS)
    // Start workers
    stageRevisions(ctx, &wg, inputChan, filterChan)

    rewriteChan = make(chan *commitItem, 100)

    // Strip out the uninteresting commits
    filterRevisions(ctx, &wg, filterChan, rewriteChan)

    // actual work
    rewriteCommits(ctx, &wg, rewriteChan)

    // wait for all work to finish
    wg.Wait()

out:

    cancel()

    if err != nil {
        fmt.Fprintf(os.Stderr, "%s\n", err)
        ret = 1
    }

    os.Exit(ret)
}
