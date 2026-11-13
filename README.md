# Git Subdirectory Filter

`git-subdir-filter` extracts the history of one subdirectory from a source
repository branch and force-pushes it to a target repository branch. The
subdirectory becomes the root of the target.

Run it once to split a project out of a monorepo, or run it on a schedule to
keep the target as a read-only mirror of the subdirectory. Each run only
processes commits added since the previous run.

## How it works

The tool rewrites each source commit so that its tree is the subdirectory's
tree. It keeps the author, committer, date, and message, and:

- Drops commits that don't change the subdirectory.
- Keeps merges where both sides changed the subdirectory.
- Collapses merges where only one side changed it.
- Records a commit that deletes the subdirectory as a commit with an empty tree.

For example, filtering `lib/foo` from this source history:

```
*   merge side          (lib/foo/side added on branch)
|\
| * side: foo/side
* | master: foo/a=2
|/
* outside prefix        (only touches README)
* add foo/a
* root                  (before lib/foo existed)
```

produces this target history, with `a` and `side` at the root:

```
*   merge side
|\
| * side: foo/side
* | master: foo/a=2
|/
* add foo/a
```

The rewrite is deterministic: the same source commit always produces the same
target commit ID. A full re-run reproduces the target exactly, which is
why incremental runs are safe.

## Usage

```sh
git-subdir-filter \
  -source-repo=git@github.com:example/monorepo.git \
  -source-branch=main \
  -filter-dir=lib/foo \
  -target-repo=git@github.com:example/foo.git \
  -target-branch=main
```

The tool fetches `main` from the monorepo into the work directory, rewrites
the history of `lib/foo`, and force-pushes the result to `main` in `foo.git`.

### Options

Each flag can also be set with an environment variable. Flags take
precedence over environment variables.

| Flag | Environment variable | Default | Description |
|------|----------------------|---------|-------------|
| `-source-repo` | `SOURCE_REPO` | required | Source repository URL. |
| `-source-branch` | `SOURCE_BRANCH` | `master` | Source branch to read. |
| `-filter-dir` | `FILTER_DIR` | required | Subdirectory to extract, for example `lib/foo`. |
| `-target-repo` | `TARGET_REPO` | required | Target repository URL. |
| `-target-branch` | `TARGET_BRANCH` | `master` | Target branch to force-push. |
| `-work-dir` | `WORKDIR` | `./work` | Bare repository that holds fetched objects and resume state. Must be empty or an existing work repository. |
| `-ssh-key` | `SSH_KEY` | first of `~/.ssh/id_ed25519`, `id_ecdsa`, `id_rsa` | Private key, tried after ssh-agent. |
| `-known-hosts` | `KNOWN_HOSTS` | `~/.ssh/known_hosts` | File used to verify SSH host keys. |
| `-insecure-skip-host-check` | `INSECURE_SKIP_HOST_CHECK` | `false` | Skip SSH host key verification. Not recommended. |
| | `SSH_PASSKEY` | | Passphrase for the private key. Environment only, so it stays out of the process list. |

The tool exits with `0` on success, `1` if the fetch, rewrite, or push fails
(including a push the target rejects, such as a protected branch), and `2`
for invalid options. The first SIGINT or SIGTERM stops a run cleanly; a second
one kills it.

### Authentication

For SSH URLs, the tool tries these methods in order:

1. ssh-agent, if `SSH_AUTH_SOCK` is set.
2. The private key from `-ssh-key`, or the default key.

The tool verifies the server's host key against `known_hosts` and refuses
to connect to unknown hosts. To add a host:

```sh
ssh-keyscan github.com >> ~/.ssh/known_hosts
```

HTTPS URLs use the system's trusted certificates.

### Run with Docker

The image contains no credentials. Run the container as your own user so it
can read your SSH files, which are usually readable only by you. Mount a host
directory as `/work` so later runs can resume:

```sh
docker build -t git-subdir-filter .

mkdir -p ~/subdir-filter-work
docker run --rm \
  --user "$(id -u):$(id -g)" -e HOME=/home/filter \
  -v ~/.ssh:/home/filter/.ssh:ro \
  -v ~/subdir-filter-work:/work \
  -e SSH_PASSKEY \
  git-subdir-filter \
  -source-repo=git@github.com:example/monorepo.git \
  -filter-dir=lib/foo \
  -target-repo=git@github.com:example/foo.git
```

`-e HOME=/home/filter` is needed because your UID has no account inside the
image; without it, the tool looks for `/.ssh`.

To use ssh-agent instead of a key file, forward the socket:

```sh
docker run --rm \
  --user "$(id -u):$(id -g)" -e HOME=/home/filter \
  -v "$SSH_AUTH_SOCK":/ssh-agent -e SSH_AUTH_SOCK=/ssh-agent \
  -v ~/.ssh/known_hosts:/home/filter/.ssh/known_hosts:ro \
  -v ~/subdir-filter-work:/work \
  git-subdir-filter -source-repo=... -filter-dir=lib/foo -target-repo=...
```

## Tracking a subdirectory

To keep a target in sync, run the command on a schedule, for example from
cron or a CI job. Each run:

1. Fetches the source branch.
2. Rewrites only the commits added since the previous run.
3. Force-pushes the target branch.

Resume state and refs are kept in the work directory, separately for each
combination of source URL, source branch, and `-filter-dir`. One work
directory can serve several jobs, including jobs that run at the same time.
If the state is lost, the next run rewrites the full history and produces the
same commit IDs.

**Important:** Don't commit directly to the target branch. Each run
force-pushes it, so any commit made there is overwritten. Make changes in the
source repository instead.

## Limitations

- Commit signatures (GPG and SSH) aren't preserved, because the rewritten
  commits have different content.
- Commit messages are written as UTF-8. A custom `encoding` header is dropped.
- The tool uses libgit2 1.5, the last version supported by
  [git2go](https://github.com/libgit2/git2go) v34. libgit2 1.5 doesn't
  include fixes for CVE-2024-24575 (revparse) and CVE-2024-24577
  (`git_index_add`). The tool doesn't call either code path: it doesn't use
  revparse and never touches the index.

## Development

```
cmd/git-subdir-filter/   Command-line entry point.
internal/config/         Flag and environment parsing.
internal/split/          History rewrite and resume state.
internal/remote/         Fetch, push, credentials, and host key checks.
```

Building requires Go 1.26 and libgit2 1.5 with SSH support. The Dockerfile
builds libgit2 from source, so the easiest way to run the checks is:

```sh
docker build --target test .
```

The `internal/split` tests compare results against `git subtree split` when
`git subtree` is installed. The builder image includes it.
