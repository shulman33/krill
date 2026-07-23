package main

import (
	"flag"
	"testing"
)

func TestParseFlexible(t *testing.T) {
	cases := []struct {
		args    []string
		wantPos []string
		wantVal string
	}{
		{[]string{"--name", "x", "dir"}, []string{"dir"}, "x"},
		{[]string{"dir", "--name", "x"}, []string{"dir"}, "x"}, // the b1 footgun
		{[]string{"a", "b", "--name", "x", "c"}, []string{"a", "b", "c"}, "x"},
		{[]string{}, nil, ""},
	}
	for _, c := range cases {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		name := fs.String("name", "", "")
		pos := parseFlexible(fs, c.args)
		if len(pos) != len(c.wantPos) {
			t.Fatalf("%v: pos = %v, want %v", c.args, pos, c.wantPos)
		}
		for i := range pos {
			if pos[i] != c.wantPos[i] {
				t.Errorf("%v: pos = %v, want %v", c.args, pos, c.wantPos)
			}
		}
		if *name != c.wantVal {
			t.Errorf("%v: name = %q, want %q", c.args, *name, c.wantVal)
		}
	}
}
