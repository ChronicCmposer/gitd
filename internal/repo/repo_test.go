package repo

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "foo", want: "foo"},
		{in: "foo.git", want: "foo"},
		{in: "foo.git.git", want: "foo.git"}, // exactly ONE trailing .git (R10-Q4)
		{in: "a.git", want: "a"},
		{in: "git", want: "git"}, // too short to be a suffix
		{in: ".git", want: ".git"},
	}
	for _, tc := range tests {
		if got := Normalize(tc.in); got != tc.want {
			t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidName(t *testing.T) {
	valid := []string{"a", "A", "repo", "my-repo", "my_repo", "my.repo", "a1", "0123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789"}
	invalid := []string{"", "-repo", ".repo", "repo!", "repo/", "repo name", "répo", "a.b.c/", "toolong" + string(make([]byte, 100))}
	for _, n := range valid {
		if !ValidName(n) {
			t.Errorf("ValidName(%q) = false, want true", n)
		}
	}
	for _, n := range invalid {
		if ValidName(n) {
			t.Errorf("ValidName(%q) = true, want false", n)
		}
	}
}

func TestIsBareRepo(t *testing.T) {
	dir := t.TempDir()
	if ok, err := IsBareRepo(dir); err != nil || ok {
		t.Fatalf("IsBareRepo(empty dir) = %v, %v; want false, nil", ok, err)
	}
	for _, name := range []string{"HEAD", "objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := IsBareRepo(dir); err != nil || !ok {
		t.Fatalf("IsBareRepo(complete) = %v, %v; want true, nil", ok, err)
	}
}

func TestRealpathUnder(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "inside.git")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := RealpathUnder(root, inside)
	if err != nil {
		t.Fatalf("RealpathUnder(inside) = %v", err)
	}
	if got != inside {
		t.Errorf("RealpathUnder = %q, want %q", got, inside)
	}

	outside := t.TempDir()
	if _, err := RealpathUnder(root, outside); err == nil {
		t.Error("RealpathUnder(outside) = nil error, want escape error")
	}
}

func TestListBare(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.git", "b.git"} {
		if err := os.MkdirAll(filepath.Join(root, name, "HEAD"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, name, "objects"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, name, "refs"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Not a gitd repo (no .git suffix) and a non-bare dir are skipped.
	if err := os.MkdirAll(filepath.Join(root, "notarepo"), 0o755); err != nil {
		t.Fatal(err)
	}
	names, err := ListBare(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Errorf("ListBare = %v, want [a b]", names)
	}
}
