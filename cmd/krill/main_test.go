package main

import "testing"

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"guestbook":        "guestbook",
		"My_Cool App":      "my-cool-app",
		"---":              "app",
		"UPPER":            "upper",
		"a.b.c":            "a-b-c",
		"this-name-is-way-too-long-for-a-dns-label-x": "this-name-is-way-too-long-for-a",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}
