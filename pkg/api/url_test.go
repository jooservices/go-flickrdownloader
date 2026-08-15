package api

import (
	"context"
	"testing"
)

func TestResolveURLHostCaseInsensitive(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"mixed-case www", "https://WWW.Flickr.com/photos/someuser/albums/72157600000000001/"},
		{"uppercase host, no www", "https://FLICKR.COM/photos/someuser/albums/72157600000000001/"},
		{"mixed-case flic.kr", "https://Flic.Kr/photos/someuser/albums/72157600000000001/"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ResolveURL(context.Background(), c.url, nil)
			if err != nil {
				t.Fatalf("ResolveURL(%q): %v", c.url, err)
			}
			if got.Type != TargetPhotoset || got.ID != "72157600000000001" {
				t.Fatalf("ResolveURL(%q) = %+v, want photoset 72157600000000001", c.url, got)
			}
		})
	}
}

func TestResolveURLRejectsNonFlickrHost(t *testing.T) {
	_, err := ResolveURL(context.Background(), "https://notflickr.com/photos/someuser/", nil)
	if err == nil {
		t.Fatal("expected an error for a non-Flickr host")
	}
}
