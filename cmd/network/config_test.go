package main

import "testing"

func TestToWSURL(t *testing.T) {
	cases := map[string]string{
		"http://localhost:8080":  "ws://localhost:8080",
		"https://example.com/":   "wss://example.com",
		"http://127.0.0.1:9000/": "ws://127.0.0.1:9000",
	}
	for in, want := range cases {
		if got := toWSURL(in); got != want {
			t.Fatalf("toWSURL(%q)=%q want %q", in, got, want)
		}
	}
}
