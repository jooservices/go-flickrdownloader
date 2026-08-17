package api

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
)

type TargetType int

const (
	TargetUser TargetType = iota
	TargetPhotoset
	TargetPhoto
)

type ParsedURL struct {
	Type TargetType
	ID   string
}

var photoIDRe = regexp.MustCompile(`^\d{8,}$`)

// nsidRe matches Flickr's owner NSID shape: digits, "@N", two-or-more digits.
var nsidRe = regexp.MustCompile(`^\d+@N\d{2,}$`)

func ResolveURL(ctx context.Context, rawURL string, client *Client) (*ParsedURL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}

	host := strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	if host != "flickr.com" && host != "flic.kr" {
		return nil, fmt.Errorf("not a Flickr URL: %s", rawURL)
	}

	cleaned := path.Clean(u.Path)
	parts := strings.Split(strings.Trim(cleaned, "/"), "/")

	if len(parts) == 0 {
		return nil, fmt.Errorf("cannot parse URL: %s", rawURL)
	}

	// /photos/{user}/albums/{id} or /photos/{user}/sets/{id}
	if parts[0] == "photos" && len(parts) >= 4 && (parts[2] == "albums" || parts[2] == "sets") {
		if parts[3] == "" {
			return nil, fmt.Errorf("cannot parse photoset ID from URL: %s", rawURL)
		}
		return &ParsedURL{Type: TargetPhotoset, ID: parts[3]}, nil
	}

	// /photos/{user}/{photo_id}
	if parts[0] == "photos" && len(parts) >= 3 && photoIDRe.MatchString(parts[2]) {
		return &ParsedURL{Type: TargetPhoto, ID: parts[2]}, nil
	}

	// /photos/{user}/ or /people/{user}/
	if parts[0] == "photos" || parts[0] == "people" {
		nsid, err := client.LookupUser(ctx, rawURL)
		if err != nil {
			return nil, fmt.Errorf("lookup user for '%s': %w", rawURL, err)
		}
		if nsid == "" {
			return nil, fmt.Errorf("could not resolve an NSID for '%s' (Flickr returned an empty user id)", rawURL)
		}
		return &ParsedURL{Type: TargetUser, ID: nsid}, nil
	}

	return nil, fmt.Errorf("unsupported Flickr URL: %s", rawURL)
}
