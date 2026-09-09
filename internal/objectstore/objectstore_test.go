package objectstore

import (
	"context"
	"errors"
	"testing"
)

func TestMemoryStorePutGet(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()

	if err := s.Put(ctx, "repos/r/a.bundle", []byte("data-a")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "repos/r/a.bundle")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "data-a" {
		t.Fatalf("Get = %q, want %q", got, "data-a")
	}
}

func TestMemoryStoreGetMissingIsNotFound(t *testing.T) {
	s := NewMemoryStore()
	_, err := s.Get(context.Background(), "repos/r/nope.bundle")
	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("Get missing = %v, want NotFoundError", err)
	}
	if nf.Key != "repos/r/nope.bundle" {
		t.Fatalf("NotFoundError.Key = %q", nf.Key)
	}
}

func TestMemoryStorePutOverwrites(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.Put(ctx, "k", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "k", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, "k")
	if string(got) != "v2" {
		t.Fatalf("Get after overwrite = %q, want %q", got, "v2")
	}
}

func TestMemoryStoreGetReturnsCopy(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	orig := []byte("immutable")
	if err := s.Put(ctx, "k", orig); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, "k")
	got[0] = 'X'
	again, _ := s.Get(ctx, "k")
	if string(again) != "immutable" {
		t.Fatalf("store mutated by caller: %q", again)
	}
}

func TestMemoryStoreListSortedByPrefix(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	for _, k := range []string{"repos/z/b.bundle", "repos/a/a.bundle", "other/x", "repos/m/m.bundle"} {
		if err := s.Put(ctx, k, []byte(k)); err != nil {
			t.Fatal(err)
		}
	}
	keys, err := s.List(ctx, "repos/")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"repos/a/a.bundle", "repos/m/m.bundle", "repos/z/b.bundle"}
	if len(keys) != len(want) {
		t.Fatalf("List = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("List = %v, want %v", keys, want)
		}
	}
}

func TestMemoryStoreListNoMatch(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.Put(ctx, "repos/r/a.bundle", []byte("x")); err != nil {
		t.Fatal(err)
	}
	keys, err := s.List(ctx, "nothing/")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("List = %v, want empty", keys)
	}
}

func TestMemoryStoreDelete(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.Put(ctx, "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "k"); !errors.As(err, new(*NotFoundError)) {
		t.Fatalf("Get after delete = %v, want NotFoundError", err)
	}
}

func TestMemoryStoreDeleteMissingIsNotError(t *testing.T) {
	s := NewMemoryStore()
	// Deleting a missing key is not an error (mirrors S3 idempotent delete).
	if err := s.Delete(context.Background(), "never-stored"); err != nil {
		t.Fatalf("Delete missing = %v, want nil", err)
	}
}

func TestNotFoundErrorMessage(t *testing.T) {
	nf := &NotFoundError{Key: "repos/r/x.bundle"}
	if got := nf.Error(); got != "object not found: repos/r/x.bundle" {
		t.Fatalf("Error() = %q", got)
	}
}
