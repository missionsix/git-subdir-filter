// Filter Branch

package main

import (
    "context"
    "errors"
    "flag"
    "fmt"
    "container/list"
    "github.com/caarlos0/env"
    "gopkg.in/libgit2/git2go.v26"
    "os"
    "os/signal"
    "sync"
    "strings"
    "syscall"
)

const (
    GIT_ORIGIN = "origin"
    // queue depth between stages
    STAGE_CHAN_DEPTH = 20
    SOURCE_WORK_BRANCH = "incoming"
    RUNTIME_CONFIG = "runtime_config"
    RUNTIME_SOURCE_REPO = "runtime_source"
    RUNTIME_TARGET_REPO = "runtime_target"
    RUNTIME_COMMIT_TREE = "commit_tree"
    FILTER_WORKERS = 24
    REWRITE_WORKERS = 24
    RESUME_TAG = "resume_filter_tag"
    RESUME_COMMIT = "resume_commit"
)


type Config struct {
    WorkDir         string `env:"FILTER_DIR" envDefault:"${PWD}/work" envExpand:"true"`
    SourceRepo      string `env:"SOURCE_REPO"`
    SourceBranch    string `env:"SOURCE_BRANCH"`
    TargetRepo      string `env:"SOURCE_REPO"`
    TargetBranch    string `env:"TARGET_BRANCH"`
    FilterDirectory string
}

type commitTree struct {
    List *list.List
    Mtx sync.Mutex
}

type commitItem struct {
    // entry in commitTree
    ListEntry *list.Element
    // Source commit
    Commit *git.Commit
    // Index
    Entry int
    // Filtered subtree
    FilterTree *git.Tree
    // Rewritten tree object
    RewriteTree *git.Tree
    // Rewrite Commit
    Rewrite *git.Commit
}

type delAddEntry struct {
    Deleted bool
    Added bool
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

func rewriteCommits(ctx context.Context) error {
    var err error
    var parent *git.Commit
    var rewrite *git.Commit

    repo := ctx.Value(RUNTIME_SOURCE_REPO).(*git.Repository)
    commits := ctx.Value(RUNTIME_COMMIT_TREE).(*commitTree)

    // Lookup the resume tag commit to use as the parent
    tagspec := fmt.Sprintf("refs/tags/%s", RESUME_TAG)
    resumeObj, _ := repo.RevparseSingle(tagspec)
    if resumeObj != nil {
        parent, _ = repo.LookupCommit(resumeObj.Id())
    }

    fmt.Println("Rewriting commits...")

    for e := commits.List.Front(); e != nil; e = e.Next() {
        select {
        case <-ctx.Done():
            return nil
        default:
            break
        }

        ci := e.Value.(*commitItem)
        if ci.RewriteTree == nil {
            continue
        }

        rewrite, err = makeCommit(repo, ci.Commit, ci.RewriteTree, parent)
        if err != nil {
            return err
        }
        parent = rewrite
        fmt.Println("New Commit", rewrite.Id())
    }

    if rewrite != nil {
        // Create/move the local branch
        _, err := repo.CreateBranch(SOURCE_WORK_BRANCH, rewrite, true)
        if err != nil {
            fmt.Println("Could not create branch from commit", rewrite.Id())
        }
    }

    // Create the resume tag from the last (most recent) filtered commit
    for e := commits.List.Back(); e != nil; e = e.Prev() {

        ci := e.Value.(*commitItem)
        if ci.RewriteTree == nil {
            continue
        }

        _, err = repo.Tags.CreateLightweight(RESUME_TAG, ci.Commit, true)
        if err != nil {
            fmt.Println("Could not create resume tag from commit", parent.Id())
        }
        break
    }

    return err
}

// TreeWalk recursively rewrites subtrees
func TreeWalk(repo *git.Repository, t *git.Tree, parent *git.TreeBuilder) (error) {
    var err error
    var oid *git.Oid
    var te *git.TreeEntry
    var idx uint64

    for idx = 0; idx < t.EntryCount(); idx++ {

        te = t.EntryByIndex(idx)

        switch te.Type {
        case git.ObjectTree:

            child, err := repo.TreeBuilder()
            if err != nil {
                return err
            }
            defer child.Free()

            subtree, err := repo.LookupTree(te.Id)
            if err != nil {
                return err
            }

            err = TreeWalk(repo, subtree, child)
            if err != nil {
                return err
            }

            oid, err = child.Write()
            if err != nil {
                return err
            }

        case git.ObjectBlob:
            oid = te.Id
            // XXX: moving objects files to stage repo requires re-writing
            // them using git.Odb
        }

        err = parent.Insert(te.Name, oid, te.Filemode)
        if err != nil {
            return err
        }
    }

    return nil
}

func rewriteTree(ctx context.Context, ci *commitItem) error {
    var oid *git.Oid

    repo := ctx.Value(RUNTIME_SOURCE_REPO).(*git.Repository)
    tree := ci.FilterTree

    tb, err := repo.TreeBuilder()
    if err != nil {
        return err
    }
    defer tb.Free()

    err = TreeWalk(repo, tree, tb)
    if err != nil {
        return err
    }

    oid, err = tb.Write()
    if err != nil {
        return err
    }

    // Assign for next stage
    rewriteTree, err := repo.LookupTree(oid)
    ci.RewriteTree = rewriteTree

    return nil
}

// rewriteWorker performs the heavy lifting of rewriting the commit tree
func treeWorker(ctx context.Context, wg *sync.WaitGroup, work <-chan *commitItem) {

    defer wg.Done()

    for {
        select {
        case <- ctx.Done():
            return
        case ci, ok := <-work:
            if !ok {
                return
            }
            err := rewriteTree(ctx, ci)
            if err != nil {
                return
            }
            fmt.Printf("%d Rewrote tree %s\n", ci.Entry, ci.FilterTree.Id())
        }
    }
}

//
func rewriteWorkers(ctx context.Context, wg *sync.WaitGroup, commits <-chan *commitItem) {
    var treeGroup sync.WaitGroup

    fmt.Printf("Starting %d tree workers...\n", REWRITE_WORKERS)
    treeGroup.Add(REWRITE_WORKERS)
    for i := 0; i < REWRITE_WORKERS; i++ {
        go treeWorker(ctx, &treeGroup, commits)
    }

    wg.Add(1)
    go func() {
        defer wg.Done()
        treeGroup.Wait()
        // wait for all work to finish
        err := rewriteCommits(ctx)
        if err != nil {
            panic(err)
        }
    }()
}


func readTree(repo *git.Repository, tree *git.Tree, filter []string) (*git.Tree, bool) {
    var err error

    te := tree.EntryByName(filter[0])
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

    if len(filter) == 1 {
        return tree, true
    }

    return readTree(repo, tree, filter[1:])
}

type StopIteration struct {
}
func (e *StopIteration) Error() string {
    return fmt.Sprintf("Iteration stopped")
}


func filterWorker(ctx context.Context, wg *sync.WaitGroup,
    filterChan <-chan *commitItem, commitChan chan<- *commitItem) {
    var err error
    var tree *git.Tree

    defer wg.Done()

    cfg := ctx.Value(RUNTIME_CONFIG).(*Config)
    repo := ctx.Value(RUNTIME_SOURCE_REPO).(*git.Repository)

    loop:
    for {
        select {
        case <- ctx.Done():
            return
        case ci, ok := <-filterChan:
            if !ok {
                // done working
                return
            }

            ci.FilterTree = nil

            tree, err = ci.Commit.Tree()
            if err != nil {
                continue loop
            }

            diffOptions := &git.DiffOptions {
                Flags: git.DiffNormal,
                IgnoreSubmodules: git.SubmoduleIgnoreAll,
                Pathspec: []string{cfg.FilterDirectory},
                MaxSize: -1,
                NotifyCallback: nil,
            }

            // filter away merge commits
            if ci.Commit.ParentCount() > 1 {
                ci.FilterTree = nil
                continue loop
            }

            // Grab the only parent tree if it exists
            var parentTree *git.Tree = nil
            if ci.Commit.ParentCount() == 1 {
                parentTree, _ = ci.Commit.Parent(0).Tree()
            }

            // diff parent and child
            diff, err := repo.DiffTreeToTree(parentTree, tree, diffOptions)
            if err != nil {
                panic(err)
            }

            // Store deletes & adds for no-op filtering
            var delAddMap map[string] *delAddEntry
            delAddMap = make(map[string]*delAddEntry)
            mapLookup := func(m *map[string]*delAddEntry, key string) *delAddEntry {
                e, ok := (*m)[key]
                if !ok {
                    e = &delAddEntry {
                        Deleted: false,
                        Added: false,
                    }
                }
                return e
            }

            // Process diff looking for usable diff entry
            diffFileCb := func(d git.DiffDelta, p float64) (git.DiffForEachHunkCallback, error) {
                var mapEntry *delAddEntry = nil
                switch d.Status {
                case git.DeltaUnmodified:
                    // ignore unmodified entries
                    return nil, nil
                case git.DeltaDeleted:
                    mapEntry = mapLookup(&delAddMap, d.NewFile.Path)
                    mapEntry.Deleted = true
                    delAddMap[d.NewFile.Path] = mapEntry
                case git.DeltaAdded:
                    mapEntry = mapLookup(&delAddMap, d.NewFile.Path)
                    mapEntry.Added = true
                    delAddMap[d.NewFile.Path] = mapEntry
                }
                // Must keep processsing diff for matching entries
                if mapEntry != nil {
                    return nil, nil
                }
                return nil, &StopIteration{}
            }

            // Iterate diff files
            err = diff.ForEach(diffFileCb, git.DiffDetailFiles)
            nonDeleteAdd := false
            for _, e := range delAddMap {
                if e.Added != e.Deleted {
                    nonDeleteAdd = true
                    break
                }
            }

            if err == nil && !nonDeleteAdd {
                // remove from consideration
                continue loop
            }
            err = nil

            // if commit tree fails to match, drop it
            filterDirs := strings.Split(cfg.FilterDirectory, "/")
            subtree, ok := readTree(repo, tree, filterDirs)
            if !ok {
                // remove element contents to be garbage collected
                continue loop
            }

            ci.FilterTree = subtree

            // blocking send the commit item to next worker
            commitChan <- ci
        }
    }
}

func filterWorkers(ctx context.Context, wg *sync.WaitGroup,
    filterChan <-chan *commitItem, commitChan chan<- *commitItem) {
    var filterGroup sync.WaitGroup

    // Strip out the uninteresting commits
    fmt.Printf("Starting %d filter workers...\n", FILTER_WORKERS)

    filterGroup.Add(FILTER_WORKERS)
    for i := 0; i < FILTER_WORKERS; i++ {
        go filterWorker(ctx, &filterGroup, filterChan, commitChan)
    }

    // deferred close of filterChan
    wg.Add(1)
    go func() {
        defer wg.Done()
        filterGroup.Wait()
        close(commitChan)
    }()
}

func stageRevisions(ctx context.Context, wg *sync.WaitGroup,
    inputChan <-chan *git.Commit, filterChan chan<- *commitItem) {

    commits := ctx.Value(RUNTIME_COMMIT_TREE).(*commitTree)

    wg.Add(1)
    go func() {
        var idx int = 0

        defer wg.Done()
        defer close(filterChan)

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
                    Entry: idx,
                    Commit: c,
                }
                idx += 1

                // returns list-element (container)
                e := commits.List.PushFront(ci)
                ci.ListEntry = e
            }
        }

        // Readers can savely observe items from filteredChan
        // and update the commits.List

        // Send ordered commits to be filtered
        for e := commits.List.Front(); e != nil; e = e.Next() {
            select {
            case <-ctx.Done():
                return
            default:
                break
            }
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
        var resumeObjId *git.Oid

        defer wg.Done()
        defer close(inputChan)

        cfg := ctx.Value(RUNTIME_CONFIG).(*Config)
        source := ctx.Value(RUNTIME_SOURCE_REPO).(*git.Repository)

        // lookup resume tag from previous iteration
        tagspec := fmt.Sprintf("refs/tags/%s", RESUME_TAG)
        resumeObj, _ := source.RevparseSingle(tagspec)
        if resumeObj != nil {
            resumeObjId = resumeObj.Id()
            fmt.Println("Found resume tag ", resumeObjId)
        }

        revwalk, err := source.Walk()
        if err != nil {
            return
        }
        defer revwalk.Free()

        // Set sort order - Topo reverse choronological
        revwalk.Sorting(git.SortTopological | git.SortTime)

        // Lookup source reference and set as start reference for walk
        refname := fmt.Sprintf("refs/remotes/%s/%s", GIT_ORIGIN, cfg.SourceBranch)
        revwalk.PushRef(refname)

        // Send commit revisions into input channel
        // in reverse chronological order
        revwalk.Iterate(func (c *git.Commit) bool {
            // stop on resume commit
            if resumeObjId != nil && *c.Id() == *resumeObjId {
                fmt.Println("Stopping on resume commit")
                return false
            }

            select {
            case <-ctx.Done():
                return false
            case inputChan <- c:
                return true
            }
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

// openRepository will create (if it doesn't exist) a git repository.
func openRepository(path string) (*git.Repository, error) {
    var err error
    var repo *git.Repository

    // open the source dir
    fmt.Println("Opening repo: ", path)

    // Create the source dir, bare
    repo, err = git.InitRepository(path, true)
    if err != nil {
        fmt.Fprintf(os.Stderr, "%s\n", err)
        os.Exit(1)
    }

    return repo, err
}

// buildEnvironment creates the set of working directories
func buildWorkEnvironment(cfg *Config) error {
    var err error

    err = os.MkdirAll(cfg.WorkDir, os.ModePerm)
    if err != nil {
        return err
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
    source, err = openRepository(cfg.WorkDir)
    if err != nil {
        goto out
    }

    // Blocking fetch
    err = fetchRemote(ctx, source)
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

    filterChan = make(chan *commitItem, FILTER_WORKERS)

    // Start revision sequencer
    stageRevisions(ctx, &wg, inputChan, filterChan)

    rewriteChan = make(chan *commitItem, REWRITE_WORKERS)

    // Start revision filters
    filterWorkers(ctx, &wg, filterChan, rewriteChan)

    // Launch commit and tree rewriters
    rewriteWorkers(ctx, &wg, rewriteChan)

    wg.Wait()

out:

    cancel()

    if err != nil {
        fmt.Fprintf(os.Stderr, "%s\n", err)
        ret = 1
    }

    os.Exit(ret)
}
