// Package split rewrites a commit graph so that one subdirectory becomes the
// repository root, preserving merges and pruning commits that don't touch it.
//
// The rewrite is deterministic: a source commit always maps to the same new
// OID given the same prefix, so incremental runs and full re-runs agree.
package split

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	git "github.com/libgit2/git2go/v34"
)

// zero marks a source commit whose history has no rewritten commit yet
// (for example, commits from before the subdirectory existed).
var zero git.Oid

// Splitter rewrites history for one prefix and persists resume state.
type Splitter struct {
	Repo   *git.Repository
	Prefix string // cleaned, e.g. "lib/foo"
	// MapPath stores "src dst" lines; StateRef records the last source tip.
	// Use StateLocations to derive both from a work directory.
	MapPath  string
	StateRef string
	Log      *slog.Logger

	mapping   map[git.Oid]git.Oid
	treeOf    map[git.Oid]git.Oid // rewritten commit -> its tree
	emptyTree *git.Oid
}

// Result summarizes one Split run.
type Result struct {
	Head    *git.Oid // rewritten tip, nil if the prefix never existed
	Visited int      // source commits walked this run
	Created int      // rewritten commits written this run
}

// StateLocations returns per-prefix resume state paths, so several prefixes
// can share one work directory. Example: ("work", "lib/foo") ->
// ("work/split-<12 hex>.map", "refs/subdir-filter/<12 hex>/source-tip").
func StateLocations(workDir, prefix string) (mapPath, stateRef string) {
	sum := sha1.Sum([]byte(prefix))
	id := hex.EncodeToString(sum[:])[:12]
	return filepath.Join(workDir, "split-"+id+".map"), "refs/subdir-filter/" + id + "/source-tip"
}

// Split rewrites every commit reachable from sourceTip that previous runs
// haven't processed, then saves resume state.
func (s *Splitter) Split(ctx context.Context, sourceTip *git.Oid) (Result, error) {
	var res Result
	log := s.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	if err := s.loadMapping(); err != nil {
		return res, err
	}
	s.treeOf = make(map[git.Oid]git.Oid)

	walk, err := s.Repo.Walk()
	if err != nil {
		return res, err
	}
	defer walk.Free()
	walk.Sorting(git.SortTopological | git.SortReverse)
	if err := walk.Push(sourceTip); err != nil {
		return res, fmt.Errorf("walk from %s: %w", sourceTip, err)
	}
	if prev := s.previousTip(); prev != nil {
		// Mapping is saved before the ref, so prev's ancestry is fully mapped.
		log.Info("resuming", "previous_tip", prev.String())
		if err := walk.Hide(prev); err != nil {
			return res, fmt.Errorf("hide %s: %w", prev, err)
		}
	}

	var id git.Oid
	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if err := walk.Next(&id); err != nil {
			if git.IsErrorCode(err, git.ErrorCodeIterOver) {
				break
			}
			return res, err
		}
		created, err := s.rewrite(&id)
		if err != nil {
			return res, fmt.Errorf("rewrite %s: %w", id.String(), err)
		}
		res.Visited++
		if created {
			res.Created++
		}
	}

	if head := s.mapping[*sourceTip]; head != zero {
		res.Head = &head
	}
	if err := s.saveMapping(); err != nil {
		return res, err
	}
	if _, err := s.Repo.References.Create(s.StateRef, sourceTip, true, "subdir-filter resume"); err != nil {
		return res, fmt.Errorf("update %s: %w", s.StateRef, err)
	}
	log.Info("split done", "visited", res.Visited, "created", res.Created)
	return res, nil
}

// rewrite maps one source commit whose parents are already mapped.
// It reports whether a new commit was written.
func (s *Splitter) rewrite(id *git.Oid) (bool, error) {
	c, err := s.Repo.LookupCommit(id)
	if err != nil {
		return false, err
	}
	defer c.Free()

	parents, err := s.rewrittenParents(c)
	if err != nil {
		return false, err
	}

	sub, err := s.subtree(c)
	if err != nil {
		return false, err
	}
	if sub == nil {
		if len(parents) == 0 {
			s.mapping[*id] = zero // prefix hasn't appeared yet
			return false, nil
		}
		// Prefix removed: record it as an empty tree so the deletion shows.
		if sub, err = s.empty(); err != nil {
			return false, err
		}
	}

	if len(parents) == 1 {
		pt, err := s.treeOfRewritten(parents[0])
		if err != nil {
			return false, err
		}
		if pt.Equal(sub) {
			s.mapping[*id] = *parents[0] // no change under prefix: prune
			return false, nil
		}
	}

	// CreateCommitFromIds writes UTF-8; a non-default encoding header is dropped.
	nid, err := s.Repo.CreateCommitFromIds("", c.Author(), c.Committer(), c.Message(), sub, parents...)
	if err != nil {
		return false, err
	}
	s.mapping[*id] = *nid
	s.treeOf[*nid] = *sub
	return true, nil
}

// rewrittenParents maps parents, dropping empty and duplicate entries and
// any parent that is an ancestor of another (a degenerate merge side).
func (s *Splitter) rewrittenParents(c *git.Commit) ([]*git.Oid, error) {
	var out []*git.Oid
	seen := make(map[git.Oid]bool)
	for i := uint(0); i < c.ParentCount(); i++ {
		p := c.ParentId(i)
		m, ok := s.mapping[*p]
		if !ok {
			return nil, fmt.Errorf("parent %s not mapped; delete %s to rebuild", p, s.MapPath)
		}
		if m == zero || seen[m] {
			continue
		}
		seen[m] = true
		mm := m
		out = append(out, &mm)
	}
	if len(out) < 2 {
		return out, nil
	}

	var kept []*git.Oid
	for i, p := range out {
		redundant := false
		for j, q := range out {
			if i == j {
				continue
			}
			isAnc, err := s.Repo.DescendantOf(q, p)
			if err != nil {
				return nil, err
			}
			if isAnc {
				redundant = true
				break
			}
		}
		if !redundant {
			kept = append(kept, p)
		}
	}
	return kept, nil
}

// subtree returns the prefix's tree OID in c, or nil if it is absent or not a directory.
func (s *Splitter) subtree(c *git.Commit) (*git.Oid, error) {
	tree, err := c.Tree()
	if err != nil {
		return nil, err
	}
	defer tree.Free()
	te, err := tree.EntryByPath(s.Prefix)
	if err != nil {
		if git.IsErrorCode(err, git.ErrorCodeNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if te.Type != git.ObjectTree {
		return nil, nil
	}
	return te.Id, nil
}

func (s *Splitter) treeOfRewritten(id *git.Oid) (*git.Oid, error) {
	if t, ok := s.treeOf[*id]; ok {
		return &t, nil
	}
	c, err := s.Repo.LookupCommit(id)
	if err != nil {
		return nil, err
	}
	defer c.Free()
	t := *c.TreeId()
	s.treeOf[*id] = t
	return &t, nil
}

func (s *Splitter) empty() (*git.Oid, error) {
	if s.emptyTree != nil {
		return s.emptyTree, nil
	}
	tb, err := s.Repo.TreeBuilder()
	if err != nil {
		return nil, err
	}
	defer tb.Free()
	id, err := tb.Write()
	if err != nil {
		return nil, err
	}
	s.emptyTree = id
	return id, nil
}

// previousTip returns the recorded source tip if the mapping covers it.
func (s *Splitter) previousTip() *git.Oid {
	ref, err := s.Repo.References.Lookup(s.StateRef)
	if err != nil {
		return nil
	}
	defer ref.Free()
	t := ref.Target()
	if t == nil {
		return nil
	}
	if _, ok := s.mapping[*t]; !ok {
		return nil // mapping lost or stale: full re-run yields the same OIDs
	}
	return t
}

func (s *Splitter) loadMapping() error {
	s.mapping = make(map[git.Oid]git.Oid)
	f, err := os.Open(s.MapPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		src, dst, ok := strings.Cut(sc.Text(), " ")
		a, errA := git.NewOid(src)
		b, errB := git.NewOid(dst)
		if !ok || errA != nil || errB != nil {
			return fmt.Errorf("%s:%d: malformed mapping line", s.MapPath, n)
		}
		s.mapping[*a] = *b
	}
	return sc.Err()
}

// saveMapping writes the mapping atomically (temp file + rename).
func (s *Splitter) saveMapping() error {
	tmp, err := os.CreateTemp(filepath.Dir(s.MapPath), filepath.Base(s.MapPath)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	w := bufio.NewWriter(tmp)
	for src, dst := range s.mapping {
		fmt.Fprintf(w, "%s %s\n", src.String(), dst.String())
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.MapPath)
}
