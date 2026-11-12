package split

import (
	"context"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	git "github.com/libgit2/git2go/v34"
)

// gitlinkPrefix marks a fixture value as a submodule commit OID.
const gitlinkPrefix = "gitlink:"

type fixture struct {
	t    *testing.T
	repo *git.Repository
	dir  string
	tick int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.InitRepository(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(repo.Free)
	return &fixture{t: t, repo: repo, dir: dir}
}

// commit writes a commit whose tree holds exactly files (path -> content).
func (f *fixture) commit(msg string, files map[string]string, parents ...*git.Oid) *git.Oid {
	f.t.Helper()
	tree := f.tree(files)
	f.tick++
	sig := &git.Signature{Name: "t", Email: "t@t", When: time.Unix(1_600_000_000+f.tick*60, 0).UTC()}
	id, err := f.repo.CreateCommitFromIds("", sig, sig, msg, tree, parents...)
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *fixture) tree(files map[string]string) *git.Oid {
	f.t.Helper()
	direct := map[string]string{}
	sub := map[string]map[string]string{}
	for p, v := range files {
		if dir, rest, ok := strings.Cut(p, "/"); ok {
			if sub[dir] == nil {
				sub[dir] = map[string]string{}
			}
			sub[dir][rest] = v
		} else {
			direct[p] = v
		}
	}
	tb, err := f.repo.TreeBuilder()
	if err != nil {
		f.t.Fatal(err)
	}
	defer tb.Free()
	for name, v := range direct {
		if oid, ok := strings.CutPrefix(v, gitlinkPrefix); ok {
			id, _ := git.NewOid(oid)
			must(f.t, tb.Insert(name, id, git.FilemodeCommit))
			continue
		}
		blob, err := f.repo.CreateBlobFromBuffer([]byte(v))
		must(f.t, err)
		must(f.t, tb.Insert(name, blob, git.FilemodeBlob))
	}
	for name, m := range sub {
		must(f.t, tb.Insert(name, f.tree(m), git.FilemodeTree))
	}
	id, err := tb.Write()
	must(f.t, err)
	return id
}

func (f *fixture) splitter(prefix string) *Splitter {
	mp, ns := StateLocations(f.dir, "src", "main", prefix)
	return &Splitter{Repo: f.repo, Prefix: prefix, MapPath: mp, StateRef: ns + "source-tip"}
}

func (f *fixture) split(prefix string, tip *git.Oid) Result {
	f.t.Helper()
	s := f.splitter(prefix)
	res, err := s.Split(context.Background(), tip)
	if err != nil {
		f.t.Fatalf("Split: %v", err)
	}
	return res
}

// history lists rewritten commits from head as "message [parent messages]" in topo order.
func (f *fixture) history(head *git.Oid) []string {
	f.t.Helper()
	if head == nil {
		return nil
	}
	w, err := f.repo.Walk()
	must(f.t, err)
	defer w.Free()
	w.Sorting(git.SortTopological | git.SortReverse)
	must(f.t, w.Push(head))
	var out []string
	var id git.Oid
	for w.Next(&id) == nil {
		c, err := f.repo.LookupCommit(&id)
		must(f.t, err)
		var ps []string
		for i := uint(0); i < c.ParentCount(); i++ {
			ps = append(ps, c.Parent(i).Summary())
		}
		sort.Strings(ps)
		out = append(out, c.Summary()+" ["+strings.Join(ps, ",")+"]")
	}
	return out
}

// files lists "path=content" in the rewritten tip tree.
func (f *fixture) files(head *git.Oid) []string {
	f.t.Helper()
	c, err := f.repo.LookupCommit(head)
	must(f.t, err)
	tree, err := c.Tree()
	must(f.t, err)
	var out []string
	must(f.t, tree.Walk(func(dir string, e *git.TreeEntry) error {
		switch e.Type {
		case git.ObjectBlob:
			b, err := f.repo.LookupBlob(e.Id)
			must(f.t, err)
			out = append(out, dir+e.Name+"="+string(b.Contents()))
		case git.ObjectCommit:
			out = append(out, dir+e.Name+"="+gitlinkPrefix+e.Id.String())
		}
		return nil
	}))
	sort.Strings(out)
	return out
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func assertEq(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s:\n got: %q\nwant: %q", what, got, want)
	}
}

func TestLinearPrunesAndTracksDeletion(t *testing.T) {
	f := newFixture(t)
	c1 := f.commit("root", map[string]string{"README": "r"})
	c2 := f.commit("add foo", map[string]string{"README": "r", "lib/foo/a": "1"}, c1)
	c3 := f.commit("outside", map[string]string{"README": "r2", "lib/foo/a": "1"}, c2)
	c4 := f.commit("edit foo", map[string]string{"README": "r2", "lib/foo/a": "2"}, c3)
	c5 := f.commit("rm foo", map[string]string{"README": "r2"}, c4)
	c6 := f.commit("readd foo", map[string]string{"README": "r2", "lib/foo/b": "b"}, c5)

	res := f.split("lib/foo", c6)
	assertEq(t, "history", f.history(res.Head), []string{
		"add foo []", "edit foo [add foo]", "rm foo [edit foo]", "readd foo [rm foo]",
	})
	assertEq(t, "files", f.files(res.Head), []string{"b=b"})
	if res.Visited != 6 || res.Created != 4 {
		t.Errorf("visited/created = %d/%d, want 6/4", res.Visited, res.Created)
	}
}

func TestMergesPreservedOrCollapsed(t *testing.T) {
	f := newFixture(t)
	base := f.commit("base", map[string]string{"lib/foo/a": "1"})
	// Both sides touch the prefix: merge kept with two parents.
	m1 := f.commit("m: foo a=2", map[string]string{"lib/foo/a": "2"}, base)
	s1 := f.commit("s: foo side", map[string]string{"lib/foo/a": "1", "lib/foo/side": "s"}, base)
	merge1 := f.commit("merge both", map[string]string{"lib/foo/a": "2", "lib/foo/side": "s"}, m1, s1)
	// Side touches only outside the prefix: merge collapses.
	s2 := f.commit("s2: outside", map[string]string{"lib/foo/a": "2", "lib/foo/side": "s", "x": "x"}, merge1)
	m2 := f.commit("m2: foo a=3", map[string]string{"lib/foo/a": "3", "lib/foo/side": "s"}, merge1)
	merge2 := f.commit("merge outside", map[string]string{"lib/foo/a": "3", "lib/foo/side": "s", "x": "x"}, m2, s2)

	res := f.split("lib/foo", merge2)
	assertEq(t, "history", f.history(res.Head), []string{
		"base []", "m: foo a=2 [base]", "s: foo side [base]",
		"merge both [m: foo a=2,s: foo side]", "m2: foo a=3 [merge both]",
	})
	assertEq(t, "files", f.files(res.Head), []string{"a=3", "side=s"})
}

func TestNestedPrefixGitlinkAndNonDirectory(t *testing.T) {
	f := newFixture(t)
	sm := "0123456789abcdef0123456789abcdef01234567"
	c1 := f.commit("file named like prefix", map[string]string{"a/b/c": "not a dir"})
	c2 := f.commit("dir with submodule", map[string]string{"a/b/c/x": "x", "a/b/c/mod": gitlinkPrefix + sm}, c1)

	res := f.split("a/b/c", c2)
	assertEq(t, "history", f.history(res.Head), []string{"dir with submodule []"})
	assertEq(t, "files", f.files(res.Head), []string{"mod=" + gitlinkPrefix + sm, "x=x"})
}

func TestPrefixNeverExists(t *testing.T) {
	f := newFixture(t)
	c1 := f.commit("root", map[string]string{"README": "r"})
	if res := f.split("lib/foo", c1); res.Head != nil {
		t.Fatalf("Head = %v, want nil", res.Head)
	}
}

func TestIncrementalIsStableAndMatchesFullRun(t *testing.T) {
	f := newFixture(t)
	c1 := f.commit("add foo", map[string]string{"lib/foo/a": "1"})
	c2 := f.commit("edit foo", map[string]string{"lib/foo/a": "2"}, c1)
	first := f.split("lib/foo", c2)

	c3 := f.commit("outside", map[string]string{"lib/foo/a": "2", "x": "x"}, c2)
	c4 := f.commit("edit again", map[string]string{"lib/foo/a": "3", "x": "x"}, c3)
	second := f.split("lib/foo", c4)
	if second.Visited != 2 || second.Created != 1 {
		t.Errorf("incremental visited/created = %d/%d, want 2/1", second.Visited, second.Created)
	}
	head, _ := f.repo.LookupCommit(second.Head)
	if !head.ParentId(0).Equal(first.Head) {
		t.Errorf("incremental parent = %s, want previous head %s", head.ParentId(0), first.Head)
	}

	// Losing resume state must reproduce identical OIDs.
	must(t, os.Remove(f.splitter("lib/foo").MapPath))
	full := f.split("lib/foo", c4)
	if !full.Head.Equal(second.Head) || full.Visited != 4 {
		t.Errorf("full re-run head %s visited %d, want %s visited 4", full.Head, full.Visited, second.Head)
	}
}

func TestMappingAppendsAndRepairsTornLine(t *testing.T) {
	f := newFixture(t)
	c1 := f.commit("add foo", map[string]string{"lib/foo/a": "1"})
	c2 := f.commit("edit foo", map[string]string{"lib/foo/a": "2"}, c1)
	first := f.split("lib/foo", c2)
	mp := f.splitter("lib/foo").MapPath
	before, err := os.ReadFile(mp)
	must(t, err)

	// Incremental run appends: existing bytes are untouched.
	c3 := f.commit("edit again", map[string]string{"lib/foo/a": "3"}, c2)
	second := f.split("lib/foo", c3)
	after, err := os.ReadFile(mp)
	must(t, err)
	if !strings.HasPrefix(string(after), string(before)) || len(after) <= len(before) {
		t.Fatalf("mapping not appended:\nbefore %q\nafter  %q", before, after)
	}

	// Simulate a crash mid-append: a partial line with no newline.
	fh, err := os.OpenFile(mp, os.O_WRONLY|os.O_APPEND, 0)
	must(t, err)
	_, err = fh.WriteString("deadbeef")
	must(t, err)
	must(t, fh.Close())
	c4 := f.commit("edit 4", map[string]string{"lib/foo/a": "4"}, c3)
	third := f.split("lib/foo", c4)
	head, err := f.repo.LookupCommit(third.Head)
	must(t, err)
	if !head.ParentId(0).Equal(second.Head) || first.Head == nil {
		t.Fatalf("after torn line: parent %s, want %s", head.ParentId(0), second.Head)
	}
	if data, _ := os.ReadFile(mp); strings.Contains(string(data), "deadbeef") {
		t.Fatal("torn line not truncated")
	}
}

func TestCancelled(t *testing.T) {
	f := newFixture(t)
	c1 := f.commit("add foo", map[string]string{"lib/foo/a": "1"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := f.splitter("lib/foo")
	if _, err := s.Split(ctx, c1); err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(s.MapPath); !os.IsNotExist(err) {
		t.Errorf("mapping written despite cancel")
	}
}

// TestMatchesGitSubtree cross-checks tip trees and commit counts against
// `git subtree split`, when available.
func TestMatchesGitSubtree(t *testing.T) {
	if out, _ := exec.Command("git", "subtree", "-h").CombinedOutput(); !strings.Contains(string(out), "split") {
		t.Skip("git subtree not installed")
	}
	histories := map[string]func(f *fixture) *git.Oid{
		"linear": func(f *fixture) *git.Oid {
			c1 := f.commit("root", map[string]string{"README": "r"})
			c2 := f.commit("add foo", map[string]string{"README": "r", "lib/foo/a": "1"}, c1)
			c3 := f.commit("outside", map[string]string{"README": "r2", "lib/foo/a": "1"}, c2)
			return f.commit("edit foo", map[string]string{"README": "r2", "lib/foo/a": "2", "lib/foo/d/b": "b"}, c3)
		},
		"merge": func(f *fixture) *git.Oid {
			base := f.commit("base", map[string]string{"lib/foo/a": "1"})
			m := f.commit("m", map[string]string{"lib/foo/a": "2"}, base)
			s := f.commit("s", map[string]string{"lib/foo/a": "1", "lib/foo/s": "s"}, base)
			return f.commit("merge", map[string]string{"lib/foo/a": "2", "lib/foo/s": "s"}, m, s)
		},
	}
	for name, build := range histories {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			tip := build(f)
			res := f.split("lib/foo", tip)
			ref, err := f.repo.References.Create("refs/heads/src", tip, true, "")
			must(t, err)
			ref.Free()

			// git subtree needs a working tree, so run it in a clone.
			wt := t.TempDir()
			git := func(dir string, args ...string) string {
				var stderr strings.Builder
				cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
				cmd.Stderr = &stderr // subtree prints progress here
				out, err := cmd.Output()
				if err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, stderr.String())
				}
				return strings.TrimSpace(string(out))
			}
			git(wt, "clone", "-q", "--branch=src", f.dir, ".")
			want := git(wt, "subtree", "split", "--prefix=lib/foo", "HEAD")

			head, err := f.repo.LookupCommit(res.Head)
			must(t, err)
			if wantTree := git(wt, "rev-parse", want+"^{tree}"); head.TreeId().String() != wantTree {
				t.Errorf("tip tree %s, git subtree %s", head.TreeId(), wantTree)
			}
			if got, want := git(f.dir, "rev-list", "--count", res.Head.String()), git(wt, "rev-list", "--count", want); got != want {
				t.Errorf("commit count %s, git subtree %s", got, want)
			}
		})
	}
}
