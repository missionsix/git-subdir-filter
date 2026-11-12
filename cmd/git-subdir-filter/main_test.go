package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenOrInit(t *testing.T) {
	empty := t.TempDir()
	repo, err := openOrInit(empty)
	if err != nil {
		t.Fatalf("empty dir: %v", err)
	}
	repo.Free()

	// Reopening the repository it created must succeed.
	repo, err = openOrInit(empty)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	repo.Free()

	// A non-empty directory that isn't a repository is never initialized.
	busy := t.TempDir()
	if err := os.WriteFile(filepath.Join(busy, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openOrInit(busy); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("non-empty dir: err = %v, want refusal", err)
	}
	if _, err := os.Stat(filepath.Join(busy, "HEAD")); err == nil {
		t.Fatal("HEAD written into unrelated directory")
	}
}
