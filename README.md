# Git Subdirectory Filter

`git-subdir-filter` is a tool which can extract a sub directory out of a
git repository.  In other words, given a _source_ respository, this tool
takes a _sub/directory/_ and creates a *Brand New* git respository at the
root of _sub/directory/_.

The motivation for this tool was that `git filter-branch --subdirectory-filter`
is extremely slow, and it does not resume on subsequent filters.

Therefore, the goals of this project:

 * Implement a performant subdirectory filter
 * Allow for subsequent calls to resume filtering

## Features

 * Auto-scaling work threads based on CPU count (configurable with `-work-threads`)
 * Auto-resume filtering based on last-filtered commit
 * Space saving - re-uses blob objects in work directory

## Requirements

 * [libgit.v27](https://github.com/libgit2/libgit2/releases/tag/v0.27.4)
 * `SSH_AGENT` must be set in your environment.
 * Source git repository branch with a subdirectory
 * A target repository (could be local)

#### Notice

The target branch *must not* be an active development branch for 
continuous filtering. This tool currently performs a _force push_
to the target.

## Usage

There are two use cases.

 * Moving a snapshot of a subdirectory into it's own repository
 * Keeping target repository in sync with a subdirectory as a read-only clone

### Snapshot a sub directory

This is basically run-once. You should run the command and then you have a
brand new target repository to work with.

It may be possible to convert the _source sub/directory_ into a git module, or
git external repository such that the _source_ repository loses ownership of
the _sub/directory_, but ends up tracking it.


### Track a sub directory

This mode is the original motivation of the tool. Using this mode assumes
the source _sub/directory_ is in active development, however there is more
active development happening elsewhere.

As such, the source repository _retains ownership_ of the _sub/directory_, and
the other development must *upstream* work to source to get it included in
the main repository.


### Example command

```sh
./git-subdir-filter -source-repo=git@github.com:missionsix/source.git
    -source-branch=master -target-repo=git@gitlab.com:missionsix/subdir.git
    -filter-dir=subdir
```

Here, `subdir` in `master.git` is being filtered out into a separate repository
`subdir.git`.
