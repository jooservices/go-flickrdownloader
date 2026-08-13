package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

type UserResponse struct {
	User struct {
		ID       string `json:"id"`
		Username struct {
			Content string `json:"_content"`
		} `json:"username"`
	} `json:"user"`
	Stat    string `json:"stat"`
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const (
	baseURL       = "https://api.flickr.com/services/rest"
	photosPerPage = 500
)

type Client struct {
	APIKey       string
	APISecret    string
	AccessToken  string
	AccessSecret string
	rateLimiter  *rate.Limiter
}

func NewClient(apiKey, apiSecret, accessToken, accessSecret string) *Client {
	return &Client{
		APIKey:       apiKey,
		APISecret:    apiSecret,
		AccessToken:  accessToken,
		AccessSecret: accessSecret,
		rateLimiter:  rate.NewLimiter(rate.Every(1050*time.Millisecond), 1),
	}
}

// flickrHint maps known Flickr error codes to actionable advice.
func flickrHint(code int, msg string) string {
	switch code {
	case 1:
		return "check the URL — the user, album or photo may not exist anymore"
	case 96, 97:
		return "your OAuth token has expired — run 'flickrdownloader auth' again"
	case 98, 99:
		return "authentication problem — run 'flickrdownloader auth' again"
	case 100:
		return "invalid API key — run 'flickrdownloader auth' again"
	case 108, 111, 114:
		return "the API call was refused (bad parameter?) — check the URL and retry"
	case 112:
		return "this API method isn't available for your key — try updating flickrdownloader"
	case 429:
		return "Flickr rate limit reached — wait a minute and retry"
	}
	if strings.Contains(strings.ToLower(msg), "rate") ||
		strings.Contains(strings.ToLower(msg), "limit") {
		return "Flickr rate limit reached — wait a minute and retry"
	}
	return ""
}

func flickrErr(code int, msg string) error {
	if h := flickrHint(code, msg); h != "" {
		return fmt.Errorf("flickr error [%d]: %s — hint: %s", code, msg, h)
	}
	return fmt.Errorf("flickr error [%d]: %s", code, msg)
}

func (c *Client) apiGet(ctx context.Context, method string, params map[string]string) ([]byte, error) {
	if err := c.rateLimiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("rate limiter: %w", err)
	}

	defaults := map[string]string{
		"method":         method,
		"format":         "json",
		"nojsoncallback": "1",
	}
	for k, v := range defaults {
		params[k] = v
	}

	return SignedGet(c.APIKey, c.APISecret, c.AccessToken, c.AccessSecret, baseURL, params)
}

func (c *Client) GetPhotosByUser(ctx context.Context, userID string, page int) (*PhotosResponse, error) {
	params := map[string]string{
		"user_id":  userID,
		"page":     fmt.Sprintf("%d", page),
		"per_page": fmt.Sprintf("%d", photosPerPage),
		"extras":   "original_format,url_o,media,o_dims",
	}

	data, err := c.apiGet(ctx, "flickr.people.getPhotos", params)
	if err != nil {
		return nil, err
	}

	var resp PhotosResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal photos: %w", err)
	}

	if resp.Stat != "ok" {
		return nil, flickrErr(resp.Code, resp.Message)
	}

	return &resp, nil
}

func (c *Client) GetPhotosByPhotoset(ctx context.Context, photosetID string, page int) (*PhotoSetResponse, error) {
	params := map[string]string{
		"photoset_id": photosetID,
		"page":        fmt.Sprintf("%d", page),
		"per_page":    fmt.Sprintf("%d", photosPerPage),
		"extras":      "original_format,url_o,media,o_dims",
	}

	data, err := c.apiGet(ctx, "flickr.photosets.getPhotos", params)
	if err != nil {
		return nil, err
	}

	var resp PhotoSetResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal photoset: %w", err)
	}

	if resp.Stat != "ok" {
		return nil, flickrErr(resp.Code, resp.Message)
	}

	return &resp, nil
}

func (c *Client) GetSizes(ctx context.Context, photoID string) (*SizesResponse, error) {
	params := map[string]string{
		"photo_id": photoID,
	}

	data, err := c.apiGet(ctx, "flickr.photos.getSizes", params)
	if err != nil {
		return nil, err
	}

	var resp SizesResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal sizes: %w", err)
	}

	if resp.Stat != "ok" {
		return nil, flickrErr(resp.Code, resp.Message)
	}

	return &resp, nil
}

func (c *Client) GetPhotosets(ctx context.Context, userID string) ([]PhotoSetInfo, error) {
	var all []PhotoSetInfo
	page := 1
	for {
		params := map[string]string{
			"user_id":  userID,
			"page":     fmt.Sprintf("%d", page),
			"per_page": "500",
		}
		data, err := c.apiGet(ctx, "flickr.photosets.getList", params)
		if err != nil {
			return nil, fmt.Errorf("photosets.getList (page %d): %w — raw: %s", page, err, string(data))
		}
		var resp PhotoSetsListResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			return nil, fmt.Errorf("unmarshal photosets list (page %d): %w — raw: %s", page, err, string(data))
		}
		if resp.Stat != "ok" {
			return nil, fmt.Errorf("%w — raw: %s", flickrErr(resp.Code, resp.Message), string(data))
		}
		all = append(all, resp.Photosets.Photoset...)
		if page >= int(resp.Photosets.Pages) {
			break
		}
		page++
	}
	return all, nil
}

func (c *Client) GetPhotosetInfo(ctx context.Context, photosetID string) (*PhotoSetInfo, error) {
	params := map[string]string{
		"photoset_id": photosetID,
	}
	data, err := c.apiGet(ctx, "flickr.photosets.getInfo", params)
	if err != nil {
		return nil, err
	}
	var resp PhotoSetInfoResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal photoset info: %w", err)
	}
	if resp.Stat != "ok" {
		return nil, flickrErr(resp.Code, resp.Message)
	}
	return &resp.Photoset, nil
}

func (c *Client) LookupUser(ctx context.Context, flickrURL string) (string, error) {
	params := map[string]string{
		"url": flickrURL,
	}
	data, err := c.apiGet(ctx, "flickr.urls.lookupUser", params)
	if err != nil {
		return "", fmt.Errorf("lookupUser: %w — raw: %s", err, string(data))
	}
	var resp UserResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("unmarshal user: %w — raw: %s", err, string(data))
	}
	if resp.Stat != "ok" {
		return "", fmt.Errorf("%w — raw: %s", flickrErr(resp.Code, resp.Message), string(data))
	}
	return resp.User.ID, nil
}

func (c *Client) GetPhotoInfo(ctx context.Context, photoID string) (*PhotoInfoResponse, error) {
	params := map[string]string{
		"photo_id": photoID,
	}
	data, err := c.apiGet(ctx, "flickr.photos.getInfo", params)
	if err != nil {
		return nil, err
	}
	var resp PhotoInfoResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal photo info: %w", err)
	}
	if resp.Stat != "ok" {
		return nil, flickrErr(resp.Code, resp.Message)
	}
	return &resp, nil
}
