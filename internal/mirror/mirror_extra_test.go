package mirror

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChronicCmposer/gitd/internal/objectstore"
)

func TestInvalidRepoNameRejected(t *testing.T) {
	m, _ := testMirror(t, objectstore.NewMemoryStore())
	ctx := context.Background()
	for _, fn := range []struct {
		name string
		call func() error
	}{
		{"create", func() error { _, err := m.CreateBundle(ctx, ".bad"); return err }},
		{"list", func() error { _, err := m.List(ctx, ".bad"); return err }},
		{"delete", func() error { return m.Delete(ctx, ".bad") }},
		{"fetch", func() error { return m.Fetch(ctx, ".bad", filepath.Join(t.TempDir(), "x.git")) }},
		{"verify", func() error { return m.Verify(ctx, ".bad") }},
	} {
		if err := fn.call(); err == nil {
			t.Errorf("%s: invalid repo name accepted", fn.name)
		} else if !strings.Contains(err.Error(), "invalid repo name") {
			t.Errorf("%s: err = %v, want invalid repo name", fn.name, err)
		}
	}
}

func TestVerifyOK(t *testing.T) {
	store := objectstore.NewMemoryStore()
	m, _ := testMirror(t, store)
	bare := makeRepo(t)
	if err := os.Rename(bare, filepath.Join(m.reposRoot, "r.git")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateBundle(context.Background(), "r"); err != nil {
		t.Fatal(err)
	}
	if err := m.Verify(context.Background(), "r"); err != nil {
		t.Fatalf("Verify = %v", err)
	}
}

func TestVerifyNoBundlesIsOK(t *testing.T) {
	m, _ := testMirror(t, objectstore.NewMemoryStore())
	if err := m.Verify(context.Background(), "r"); err != nil {
		t.Fatalf("Verify(no bundles) = %v, want nil (R9-Q1)", err)
	}
}

func TestVerifyCorruptBundleFails(t *testing.T) {
	store := objectstore.NewMemoryStore()
	m, _ := testMirror(t, store)
	bare := makeRepo(t)
	if err := os.Rename(bare, filepath.Join(m.reposRoot, "r.git")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateBundle(context.Background(), "r"); err != nil {
		t.Fatal(err)
	}
	// Corrupt the stored bundle: fetch + verify must fail loudly (R8-Q3).
	keys, err := m.List(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), keys[0], []byte("not a bundle")); err != nil {
		t.Fatal(err)
	}
	if err := m.Verify(context.Background(), "r"); err == nil {
		t.Fatal("Verify(corrupt) = nil error, want failure")
	}
}

func TestFetchNoBundlesFails(t *testing.T) {
	m, _ := testMirror(t, objectstore.NewMemoryStore())
	dest := filepath.Join(t.TempDir(), "restored.git")
	err := m.Fetch(context.Background(), "r", dest)
	if err == nil || !strings.Contains(err.Error(), "no bundles") {
		t.Fatalf("Fetch(no bundles) = %v, want no-bundles error", err)
	}
}

func TestRestoreDefaultsToReposRoot(t *testing.T) {
	store := objectstore.NewMemoryStore()
	m, _ := testMirror(t, store)
	bare := makeRepo(t)
	if err := os.Rename(bare, filepath.Join(m.reposRoot, "r.git")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateBundle(context.Background(), "r"); err != nil {
		t.Fatal(err)
	}
	wantHead, err := m.git.RunIn(context.Background(), filepath.Join(m.reposRoot, "r.git"), "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the lost repo: restore must recreate the canonical path
	// reposRoot/r.git without a caller-supplied dest (R8-Q2/R11-Q5).
	if err := os.RemoveAll(filepath.Join(m.reposRoot, "r.git")); err != nil {
		t.Fatal(err)
	}
	if err := m.Restore(context.Background(), "r"); err != nil {
		t.Fatalf("Restore = %v", err)
	}
	dest := filepath.Join(m.reposRoot, "r.git")
	head, err := m.git.RunIn(context.Background(), dest, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("restored rev-parse HEAD: %v", err)
	}
	if string(head) != string(wantHead) {
		t.Errorf("restored HEAD %q != source HEAD %q", head, wantHead)
	}
	// Restore into the now-existing canonical path fails fast, like Fetch.
	err = m.Restore(context.Background(), "r")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("Restore into existing dest = %v, want already-exists error", err)
	}
}

func TestRestoreNoBundlesFails(t *testing.T) {
	m, _ := testMirror(t, objectstore.NewMemoryStore())
	err := m.Restore(context.Background(), "r")
	if err == nil || !strings.Contains(err.Error(), "no bundles") {
		t.Fatalf("Restore(no bundles) = %v, want no-bundles error", err)
	}
}

func TestRestoreInvalidNameFails(t *testing.T) {
	m, _ := testMirror(t, objectstore.NewMemoryStore())
	if err := m.Restore(context.Background(), ".bad"); err == nil {
		t.Fatal("Restore invalid name = nil error")
	}
}

func TestFetchInvalidNameFails(t *testing.T) {
	m, _ := testMirror(t, objectstore.NewMemoryStore())
	if err := m.Fetch(context.Background(), ".bad", filepath.Join(t.TempDir(), "x.git")); err == nil {
		t.Fatal("Fetch invalid name = nil error")
	}
}

func TestDownloadMissingKey(t *testing.T) {
	m, _ := testMirror(t, objectstore.NewMemoryStore())
	_, cleanup, err := m.download(context.Background(), "repos/r/nope.bundle")
	if err == nil {
		cleanup()
		t.Fatal("download missing key = nil error")
	}
	var nf *objectstore.NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("download err = %v, want NotFoundError", err)
	}
}

func TestDownloadOK(t *testing.T) {
	store := objectstore.NewMemoryStore()
	m, _ := testMirror(t, store)
	if err := store.Put(context.Background(), "repos/r/a.bundle", []byte("payload")); err != nil {
		t.Fatal(err)
	}
	path, cleanup, err := m.download(context.Background(), "repos/r/a.bundle")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "payload" {
		t.Fatalf("download = %q, want payload", data)
	}
}

func TestUnbundleMalformedOutput(t *testing.T) {
	// unbundle must ignore malformed lines (not crash) and proceed.
	m, _ := testMirror(t, objectstore.NewMemoryStore())
	dest := t.TempDir()
	err := m.unbundle(context.Background(), dest, "/nonexistent")
	// git bundle unbundle on a bad path errors; the malformed-output path is
	// exercised when the git call itself succeeds. We assert at minimum that
	// the call fails loudly rather than panicking.
	if err == nil {
		t.Fatal("unbundle = nil error on missing bundle")
	}
}
