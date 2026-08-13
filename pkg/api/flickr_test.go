package api

import (
	"strings"
	"testing"
)

func TestFlickrHint(t *testing.T) {
	cases := []struct {
		code    int
		msg     string
		wantSub string
	}{
		{1, "User not found", "check the URL"},
		{98, "Login failed", "flickrdownloader auth"},
		{99, "User not logged in", "flickrdownloader auth"},
		{100, "Invalid API Key", "flickrdownloader auth"},
		{112, "Method not found", "updating flickrdownloader"},
		{429, "", "rate limit"},
		{0, "Rate Limit Exceeded", "rate limit"},
		{3, "some obscure error", ""},
	}
	for _, c := range cases {
		got := flickrHint(c.code, c.msg)
		if c.wantSub == "" {
			if got != "" {
				t.Errorf("flickrHint(%d, %q) = %q, want empty", c.code, c.msg, got)
			}
			continue
		}
		if !strings.Contains(strings.ToLower(got), strings.ToLower(c.wantSub)) {
			t.Errorf("flickrHint(%d, %q) = %q, want it to mention %q", c.code, c.msg, got, c.wantSub)
		}
	}
}

func TestFlickrErr(t *testing.T) {
	err := flickrErr(98, "Login failed")
	if err == nil || !strings.Contains(err.Error(), "hint") {
		t.Fatalf("expected error with hint, got %v", err)
	}
	err = flickrErr(3, "Unknown thing")
	if err == nil || strings.Contains(err.Error(), "hint") {
		t.Fatalf("expected plain error without hint, got %v", err)
	}
}
