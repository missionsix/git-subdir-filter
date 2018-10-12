// Filter Branch

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
    RUNTIME_TARGET_REPO = "runtime_target"
    RUNTIME_COMMIT_TREE = "commit_tree"
    RUNTIME_WORKERS = 4
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
    // Source commit
    Commit *git.Commit
    // Filtered subtree
    FilterTree *git.Tree
    // Rewritten tree object
    RewriteTree *git.Tree
    // Rewrite Commit
    Rewrite *git.Commit
}

func makeCommit(repo *git.Repository, c *git.Commit, t *git.Tree, p *git.Commit) (*git.Commit, error) {
    var err error
    var rewrite *git.Commit
    var oid *git.Oid

    var parents []*git.Commit
    if p != nil {
        parents = append(parents, p)
    }

    // rewrite the commit with the subtree
    oid, err = repo.CreateCommit(
        "", // refname
        c.Author(),
        c.Committer(),
        c.Message(),
        t, // subdir tree
        parents...)

    rewrite, err = repo.LookupCommit(oid)

    return rewrite, err
}

func rewriteCommits(ctx context.Context, wg *sync.WaitGroup) {
    var parent *git.Commit
    var count int = 0

    repo := ctx.Value(RUNTIME_TARGET_REPO).(*git.Repository)
    commits := ctx.Value(RUNTIME_COMMIT_TREE).(*commitTree)

    // Block for synchrous cleanup
    wg.Wait()

    fmt.Println("Rewriting commits...")

    for e := commits.List.Front(); e != nil; e = e.Next() {
        select {
        case <-ctx.Done():
            return
        default:
            break
        }

        ci := e.Value.(*commitItem)
        if ci.Commit == nil {
            continue
        }

        rewrite, err := makeCommit(repo, ci.Commit, ci.RewriteTree, parent)
        if err != nil {
            panic(err)
        }
        parent = rewrite
        fmt.Println(count, rewrite.Id())
        count += 1
    }

    if parent != nil {
        _, err := repo.CreateBranch(SOURCE_WORK_BRANCH, parent, true)
        if err != nil {
            fmt.Println("Could not create branch from commit", parent.Id())
        }
    }
}

func rewriteCommit(ctx context.Context, ci *commitItem) {
    // recursively build TreeBuilder
    /*
     tb, err := TreeBuilder()

    tree.Walk(func(entry string, t *git.TreeEntry) {
       tb.Insert()
    })
     */
    fmt.Println(ci.Commit.Id())
}

// rewriteWorker performs the heavy lifting of rewriting the commit tree
func rewriteWorker(ctx context.Context, wg *sync.WaitGroup, work <-chan *commitItem) {

    defer wg.Done()

    for {
        select {
        case <- ctx.Done():
            return
        case ci, ok := <-work:
            if !ok {
                return
            }
            rewriteCommit(ctx, ci)
        }
    }
}

func readTree(repo *git.Repository, tree *git.Tree, filter []string, n int) (*git.Tree, bool) {
    var err error

    if len(filter) == n {
        return tree, true
    }

    te := tree.EntryByName(filter[n])
    if te == nil {
        return nil, false
    }

    if te.Type != git.ObjectTree {
        return nil, false
    }

    tree, err = repo.LookupTree(te.Id)
    if err != nil {
        return nil, false
    }

    return readTree(repo, tree, filter, n+1)
}

func filterWorker(ctx context.Context, wg *sync.WaitGroup,
    filterChan <-chan *commitItem, commitChan chan<- *commitItem) {
    var err error
    var tree *git.Tree

    defer wg.Done()

    cfg := ctx.Value(RUNTIME_CONFIG).(*Config)
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
            tree, ok := readTree(repo, tree, filterDirs, 0)
            if !ok {
                // remove element contents to be garbage collected
                ci.Commit = nil
                ci.FilterTree = nil
                continue
            }

            ci.FilterTree = tree
            // non-blocking send the commit item to next worker
            select {
            case commitChan <- ci:
            }
        }
    }
}

func filterRevisions(ctx context.Context, wg *sync.WaitGroup,
    filterChan <-chan *commitItem, commitChan chan<- *commitItem) {
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
    var source *git.Repository
    var target *git.Repository
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
    source, err = openRepository(ctx, cfg.SourceDir)
    if err != nil {
        goto out
    }

    // Blocking fetch
    err = fetchRemote(ctx, source)
    if err != nil {
        goto out
    }


    target, err = openRepository(ctx, cfg.StageDir)
    if err != nil {
        goto out
    }


    // Create work structures
    commits = &commitTree {
        List: list.New(),
    }

    ctx = context.WithValue(ctx, RUNTIME_SOURCE_REPO, source)
    ctx = context.WithValue(ctx, RUNTIME_COMMIT_TREE, commits)

    // Hand off channel (mvar)
    inputChan = make(chan *git.Commit, 1000)

    // Launch sequency input processing
    readInputRevisions(ctx, &wg, inputChan)

    filterChan = make(chan *commitItem, 1000)

    // Start workers
    stageRevisions(ctx, &wg, inputChan, filterChan)

    rewriteChan = make(chan *commitItem, 1000)

    // Strip out the uninteresting commits
    fmt.Printf("Starting %d filter workers...\n", RUNTIME_WORKERS)

    wg.Add(RUNTIME_WORKERS)
    for i := 0; i < RUNTIME_WORKERS; i++ {
        go filterWorker(ctx, &wg, filterChan, rewriteChan)
    }

    ctx = context.WithValue(ctx, RUNTIME_TARGET_REPO, target)

    fmt.Printf("Starting %d rewrite  workers...\n", RUNTIME_WORKERS)
    wg.Add(RUNTIME_WORKERS)
    for i := 0; i < RUNTIME_WORKERS; i++ {
        go rewriteWorker(ctx, &wg, rewriteChan)
    }

    // wait for all work to finish
    rewriteCommits(ctx, &wg)

out:

    cancel()

    if err != nil {
        fmt.Fprintf(os.Stderr, "%s\n", err)
        ret = 1
    }

    os.Exit(ret)
}
