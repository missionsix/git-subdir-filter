# Git Filter Branch

`git/git-filter-branch.sh`

## Option parsing

`--subdirectory-filter=`

* VAR: `filter_subdir`
* VAR: `remap_to_ancestor=t`

## Environment setup

* make temp dir `mkdir -p $tempdir/t` && `cd $tempdir/t`
* `ORIG_GIT_DIR=$GIT_DIR` .. (more env)
* `GIT_WORK_TREE=.` and `export GIT_DIR GIT_WORK_TREE`
* check for previous work `refs/original`
* `GIT_INDEX_FILE=$(pwd)/../index` export `GIT_INDEX_FILE`
* Create map dir: `mkdir -p ../map`

### Create backup of references

```sh
git for-each-ref > "$tempdir/backup-refs"
```

`go-git` version:
```golang
worktree := ...
r, err := gogit.Repository.Open("", worktree)
if err != nil {
    // fatal
}

// Iterate refs
i, err := r.References()
if err != nil {
   // fatal
}

i.ForEach(func(r *pumbling.Reference) error {
    // do something with r
})
```

### Resume work

Check state branch:
```sh
git rev-parse --no-flags --revs-only $state_branch`
if success {
    // populate map from $state_branch (perl)
}
```

### Generate parse list

generate head HASH example:
```sh
for each HEAD:
    git rev-parse --no-flags --revs-only --symbolic-full-name --default HEAD
    git rev-parse --verify HEAD^0 >> $(pwd)/../heads
```

get the parse revs:
```sh
git rev-parse --revs-only "$@" > ../parse
```


### Test the subdirectory filter

```sh
git rev-list --sq --no-revs "$@" -- $filter_subdir
```
Then, needs index: `need_index=t`

### Generate the revlist
use `<branch>` and `../parse` file to generate the reverse commit list
```
git rev-list --reverse --topo-order --default HEAD \
    --parents --simplify-merges --stdin <branch> < ../parse > ../revs

commits=$(wc -l < ../revs | tr -d " ")
```

### Process revs

Iterate though each revision in `../revs`, write output to `../commit`.

`$commit:$filter_subdir` is a _git tree-ish_
```sh
for commit,parents in < ../revs :
    GIT_ALLOW_NULL_SHA1=1
    # Read the commit object into the index based on the tree-ish
    git read-tree -i -m $commit:"$filter_subdir" 2>&1
    if !success {
        # Verify tree-ish is in the database
        git rev-parse -q --verify $commit:"$filter_subdir"
        if !success {
            err
        }
        # clear the stage
        rm -f "$GIT_INDEX_FILE"         
    }

    # Stage the current commit object
    GIT_COMMIT=$commit
    export GIT_COMMIT  
    git cat-file commit "$commit" > ../commit

    for parent in parents:
        $parentstr = "-p $parent"
    
    # Build message
    msg = skip_header(../commit)
    filter_msg(msg) > ../message

    # write the tree object
    tree=$(git write-tree)

    workdir = git commit-tree $tree $parentstr < ../message  > ../map/$commit
```

### Remap ancestor(s)

Must filter non-existant commits and fix references

```sh
for each ref in <heads:
    sha1=git rev-parse $ref
    if !../map/sha1 {
        continue
    }
    ancestor = git rev-parse --simplify-merges -1 $ref <branch>
    store ancestor(s) in ref/$sha1
```

Update the refs
```sh
foreach ref in ../heads
    sha1 = git rev-parse $ref
    rewritten = map $sha1
    if $rewritten == $sha1:
        continue
    if $rewritten == '':
        # ref deleted
        git update-ref -m 'filter-branch: delete' -d $ref $sha1
    if single_ref($rewritten):
        git update-ref -m 'filter-branch: rewrite' $ref $rewritten $sha1
    else:
        # update to first
        git update-ref -m 'filter-branch: rewrite to first' $ref $rewritten $sha1
    # create backup
    git update-ref -m 'filter-branch: backup' '$orig$ref' $sha1
```

### save state branch

```sh
with open(../map):
    # generate state blob
    # save state to filter.map
    # git mktree
    # commit ref
```

### validate

```sh
git read-tree -u -m HEAD
```

