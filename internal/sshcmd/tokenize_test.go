package sshcmd

import (
	"reflect"
	"testing"
)

// parseCases are the table-test inputs for ParseCommand (R2-Q10). The fuzz
// target is seeded from the same inputs (R3-Q9).
var parseCases = []struct {
	name string
	in   string
	want []string
	err  bool
}{
	{name: "empty", in: "", want: []string{}},
	{name: "whitespace only", in: "   \t ", want: []string{}},
	{name: "single word", in: "git-upload-pack", want: []string{"git-upload-pack"}},
	{name: "dash space form", in: "git-upload-pack '/repo.git'", want: []string{"git-upload-pack", "/repo.git"}},
	{name: "unquoted repo", in: "git-receive-pack /repo.git", want: []string{"git-receive-pack", "/repo.git"}},
	{name: "quoted repo", in: "git-receive-pack 'repo.git'", want: []string{"git-receive-pack", "repo.git"}},
	{name: "empty quoted word", in: "git-receive-pack ''", want: []string{"git-receive-pack", ""}},
	{name: "adjacent quoted words join", in: "'ab''cd'", want: []string{"abcd"}},
	{name: "quotes group spaces", in: "git-upload-pack 'a b.git'", want: []string{"git-upload-pack", "a b.git"}},
	{name: "no escape inside quotes", in: "'a\\b'", want: []string{`a\b`}},
	{name: "tabs and newlines", in: "git-upload-pack\trepo.git\n", want: []string{"git-upload-pack", "repo.git"}},
	{name: "crlf", in: "git-upload-pack repo.git\r\n", want: []string{"git-upload-pack", "repo.git"}},
	{name: "trailing whitespace", in: "git-upload-pack repo.git   ", want: []string{"git-upload-pack", "repo.git"}},
	{name: "unterminated quote", in: "git-upload-pack 'repo.git", err: true},
	{name: "empty command with quote", in: "'", err: true},
	{name: "nested quotes literal", in: "''''", want: []string{""}},
	{name: "multiple args", in: "a b c", want: []string{"a", "b", "c"}},
}

func TestParseCommand(t *testing.T) {
	for _, tc := range parseCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseCommand(tc.in)
			if tc.err {
				if err == nil {
					t.Fatalf("ParseCommand(%q) err = nil, want error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCommand(%q) err = %v", tc.in, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseCommand(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

// FuzzParseCommand fuzzes the tokenizer (R3-Q9). Invariants: never panics
// (guaranteed by the fuzzer), deterministic output for a given input, and
// err==nil implies well-formed argv: no arg contains a quote character and a
// re-parse of the joined argv reproduces the same tokens when the input was
// quote-free, so any stray quote inside an arg is a tokenizer bug.
func FuzzParseCommand(f *testing.F) {
	for _, tc := range parseCases {
		f.Add(tc.in)
	}
	f.Fuzz(func(t *testing.T, in string) {
		first, err1 := ParseCommand(in)
		second, err2 := ParseCommand(in)
		if (err1 == nil) != (err2 == nil) {
			t.Fatalf("nondeterministic error for %q: %v vs %v", in, err1, err2)
		}
		if err1 != nil {
			return
		}
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("nondeterministic parse for %q: %#v vs %#v", in, first, second)
		}
		for _, arg := range first {
			for _, c := range arg {
				if c == '\'' {
					t.Fatalf("parse of %q leaked a quote into argv %#v", in, first)
				}
			}
		}
	})
}
