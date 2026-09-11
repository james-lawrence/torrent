// Package bep0019 implements BEP19 HTTP/FTP webseeding (the GetRight-style
// url-list protocol): fetching torrent data via HTTP range requests against
// mirrors listed in a torrent's url-list / a magnet's ws= parameters, rather
// than from BitTorrent peers.
package bep0019

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/james-lawrence/torrent/internal/netx"
	"github.com/james-lawrence/torrent/metainfo"
)

// URL builds the location of a specific file within a webseed as defined by
// BEP19: for a single-file torrent the seed URL points directly at the file;
// for a multi-file torrent the seed URL is treated as a directory and the
// file's path (info.Name + the file's path components) is appended to it.
func URL(root string, info *metainfo.Info, file metainfo.FileInfo) (string, error) {
	if !info.IsDir() {
		return root, nil
	}

	parts := make([]string, 0, len(file.Path)+1)
	parts = append(parts, info.Name)
	parts = append(parts, file.Path...)

	escaped := make([]string, len(parts))
	for i, p := range parts {
		escaped[i] = url.PathEscape(p)
	}

	return strings.TrimSuffix(root, "/") + "/" + strings.Join(escaped, "/"), nil
}

// NewRangeRequest builds an HTTP GET for the half-open byte range
// [start, start+length) of url.
func NewRangeRequest(ctx context.Context, rawurl string, start, length int64) (*http.Request, error) {
	if length <= 0 {
		return nil, fmt.Errorf("bep0019: invalid range length %d", length)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, start+length-1))
	return req, nil
}

// ParseRangeResponse validates that resp represents a satisfied byte-range
// request and returns a reader for exactly the requested bytes.
func ParseRangeResponse(resp *http.Response) (io.Reader, error) {
	switch resp.StatusCode {
	case http.StatusPartialContent:
		return resp.Body, nil
	case http.StatusOK:
		return nil, fmt.Errorf("bep0019: webseed ignored range request (got 200 OK)")
	default:
		return nil, fmt.Errorf("bep0019: unexpected response status: %s", resp.Status)
	}
}

// Client fetches byte ranges from webseed HTTP servers.
type Client struct {
	hc *http.Client
}

// NewClient builds a Client that dials through d - typically the same
// dialer configured for the rest of the torrent Client - so webseed traffic
// honors the same proxy/binding/resolution behavior as tracker and peer
// connections.
func NewClient(d netx.Dialer, timeout time.Duration) *Client {
	return &Client{
		hc: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				DialContext:         d.DialContext,
				Proxy:               http.ProxyFromEnvironment,
				TLSHandshakeTimeout: 15 * time.Second,
			},
		},
	}
}

// Fetch retrieves the half-open byte range [start, start+length) from rawurl.
func (c *Client) Fetch(ctx context.Context, rawurl string, start, length int64) ([]byte, error) {
	req, err := NewRangeRequest(ctx, rawurl, start, length)
	if err != nil {
		return nil, err
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := ParseRangeResponse(resp)
	if err != nil {
		return nil, err
	}

	buf := make([]byte, length)
	if _, err := io.ReadFull(body, buf); err != nil {
		return nil, fmt.Errorf("bep0019: reading range response: %w", err)
	}

	return buf, nil
}
