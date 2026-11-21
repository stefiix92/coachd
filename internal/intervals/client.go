// Package intervals is a minimal client for the intervals.icu API.
package intervals

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/stefiix92/coachd/internal/civil"
)

// DefaultBaseURL is the intervals.icu host.
const DefaultBaseURL = "https://intervals.icu"

const (
	maxListBody       = 16 << 20
	maxFileBody       = 64 << 20 // compressed and decompressed
	defaultRetryAfter = 15 * time.Minute
	maxDrain          = 1 << 20
)

// Client talks to the intervals.icu API. It is safe for concurrent use.
type Client struct {
	base      string
	key       string
	userAgent string
	hc        *http.Client
	now       func() time.Time
}

// New builds a client. A nil hc gets a 30 s timeout.
func New(baseURL, apiKey, userAgent string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{
		base:      strings.TrimRight(baseURL, "/"),
		key:       apiKey,
		userAgent: userAgent,
		hc:        hc,
		now:       time.Now,
	}
}

// get performs one GET and returns the 2xx body, capped at maxBody. Other
// statuses become ErrUnauthorized, ErrNotFound, *RateLimitedError or
// *StatusError; none of them carries the URL, the key or a response body.
func (c *Client) get(ctx context.Context, endpoint, path string, maxBody int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, &StatusError{Endpoint: endpoint}
	}
	req.SetBasicAuth("API_KEY", c.key)
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, &StatusError{Endpoint: endpoint} // never wrap: url.Error embeds the URL
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
		return nil, ErrUnauthorized
	case resp.StatusCode == http.StatusNotFound:
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
		return nil, ErrNotFound
	case resp.StatusCode == http.StatusTooManyRequests:
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
		return nil, c.rateLimited(resp.Header)
	default:
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
		return nil, &StatusError{Endpoint: endpoint, Status: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, &StatusError{Endpoint: endpoint}
	}
	if int64(len(body)) > maxBody {
		return nil, fmt.Errorf("intervals: %s: response too large", endpoint)
	}
	return body, nil
}

func (c *Client) rateLimited(h http.Header) *RateLimitedError {
	now := c.now()
	if secs, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After"))); err == nil && secs >= 0 {
		return &RateLimitedError{RetryAt: now.Add(time.Duration(secs) * time.Second)}
	}
	return &RateLimitedError{RetryAt: now.Add(defaultRetryAfter)}
}

// ListActivities returns the activities from..to (inclusive), each with its
// raw JSON.
func (c *Client) ListActivities(ctx context.Context, from, to civil.Date) ([]Item, error) {
	q := url.Values{"oldest": {from.String()}, "newest": {to.String()}}
	body, err := c.get(ctx, "activities", "/api/v1/athlete/0/activities?"+q.Encode(), maxListBody)
	if err != nil {
		return nil, err
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(body, &raws); err != nil {
		return nil, fmt.Errorf("intervals: activities: decode failed")
	}
	items := make([]Item, 0, len(raws))
	for _, r := range raws {
		var a Activity
		if err := json.Unmarshal(r, &a); err != nil {
			return nil, fmt.Errorf("intervals: activities: decode failed")
		}
		items = append(items, Item{V: a, Raw: r})
	}
	return items, nil
}

// DownloadFile returns the activity's original file, decompressed. A body
// that starts with the gzip magic is gunzipped; any other body is returned
// as is. A file over 64 MiB, compressed or decompressed, is an error.
func (c *Client) DownloadFile(ctx context.Context, id string) ([]byte, error) {
	return c.download(ctx, "file", id)
}

// DownloadGeneratedFIT returns the FIT file intervals.icu builds from the
// activity's streams, for an activity whose original is not a FIT file. Body
// handling and limits are DownloadFile's.
func (c *Client) DownloadGeneratedFIT(ctx context.Context, id string) ([]byte, error) {
	return c.download(ctx, "fit-file", id)
}

func (c *Client) download(ctx context.Context, endpoint, id string) ([]byte, error) {
	body, err := c.get(ctx, endpoint, "/api/v1/activity/"+url.PathEscape(id)+"/"+endpoint, maxFileBody)
	if err != nil {
		return nil, err
	}
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		return body, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("intervals: %s: bad gzip", endpoint)
	}
	out, err := io.ReadAll(io.LimitReader(zr, maxFileBody+1))
	if err != nil {
		return nil, fmt.Errorf("intervals: %s: bad gzip", endpoint)
	}
	if int64(len(out)) > maxFileBody {
		return nil, fmt.Errorf("intervals: %s: response too large", endpoint)
	}
	return out, nil
}

// ActivityExists reports whether the activity is still there. It is false with
// a nil error only on a 404.
func (c *Client) ActivityExists(ctx context.Context, id string) (bool, error) {
	_, err := c.get(ctx, "activity", "/api/v1/activity/"+url.PathEscape(id), maxListBody)
	if err == ErrNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
