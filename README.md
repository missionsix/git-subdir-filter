# Git Subdirectory Filter

`git-subdir-filter` is a command line tool which extracts a given sub directory
from a source git repository into a separate repository.  In other words, given
a _source_ respository, this tool takes a _sub/directory/_ argument and posts
the contents from the root of _sub/directory_ to a *secondary* repository.

## Use Cases

There are two main use cases.

 * Moving a snapshot of a subdirectory into it's own git repository
 * Keeping a target git repository in sync with a source's subdirectory as a read-only shadow clone

Creating a new git repository allows for sub-project isolation where as tracking
upstream changes could allow project owners to distribute independent parts of a
larger project.

## Motivation

The motivation for developing this tool was to solve a business use case, after
not finding and adequite tool for the job. This led to the ideation of a more
performant version of `git filter-branch --subdirectory-filter` and to address
some its shortcomings.

Thus, the development goals of this project became:

 * Implement a performant subdirectory filter command
 * Allow for subsequent commands from the same source directory to resume filtering for tracking active development.

## Command Features

 * Auto-scaling work threads based on CPU count (configurable with `-work-threads`)
 * Auto-resume filtering based on last-filtered commit
 * Uses a single working directory
 * Space saving - re-uses blob objects in working directory
 * Force-update of target repository

## Build Requirements

 * [libgit.v27](https://github.com/libgit2/libgit2/releases/tag/v0.27.4)

#### Notice

The target branch *must not* be an active development branch for 
continuous filtering. This tool currently performs a _force push_
to the target.

## Usage

### Requirements 

The following is a list of requirements for running `git-subdir-filter`

 * A _source_ git repository branch with a desired _sub/directory_
 * A _target_ repository (can be local)
 * `rsa` keys must be in the standard `$USER/.ssh/` location
 * `SSH_PASSKEY` must be set if your private key needs unlocking


### Snapshot a sub directory

This is basically run-once. You should run the command and then you have a
brand new target git repository to develop on.

*Note* It may be possible to convert the _source sub/directory_ into a git
module, or git external repository such that the _source_ repository loses
ownership of the _sub/directory_ and ends up tracking the new target.

### Track a sub directory

This mode is the original motivation of the tool. Using this mode assumes
the _source sub/directory_ is in active development.  The _target_ becomes
a "clone-only" repository, and may be distributed.

In this model, the source repository _retains ownership_ of the _sub/directory_,
and any development, from the git _source_ *OR* a _target_ clone,
*MUST* _upstream_ git commits to the source repository for it to flow down into 
the new _target_.


The command should run periodically, ideally scheduled with a chron job, such
that the tracking repository (_target_) does not drift.

### Example command

```sh
./git-subdir-filter -source-repo=git@github.com:missionsix/source.git
    -source-branch=master -target-repo=git@gitlab.com:missionsix/subdir.git
    -filter-dir=subdir
```

In this example `subdir` in the repository `github.com:missionsix/source.git` is
to be extracted and pushed to the target repository
`github.com:missionsix/subdir.git`.

