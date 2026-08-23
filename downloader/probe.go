// Package downloader implements a concurrent, resumable HTTP downloader.
// Files are fetched in byte-range chunks by a worker pool writing to a
// shared preallocated file with positional WriteAt.
package downloader

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// ProbeResult describes what the server reported about a URL: the total
// content size, whether byte-range requests are supported, and any
// validators (ETag, Last-Modified) usable to detect content changes
// between download sessions.
type ProbeResult struct {
	Size            int64
	RangesSupported bool
	ETag            string
	LastModified    string
}

// Probe issues a GET with "Range: bytes=0-0" to discover the resource's
// size, range support, and validators. A GET with a one-byte range is more
// reliable than HEAD across servers. A 206 response with a Content-Range
// total confirms range support; a 200 response means the server ignored
// the Range header, so only a single-stream download is possible.
func Probe(ctx context.Context, client *http.Client, url string) (ProbeResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ProbeResult{}, err
	}
	req.Header.Set("Range", "bytes=0-0")

	resp, err := client.Do(req)
	if err != nil {
		return ProbeResult{}, err
	}
	defer resp.Body.Close()

	res := ProbeResult{
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
	}

	switch resp.StatusCode {
	case http.StatusPartialContent:
		size, err := parseContentRangeTotal(resp.Header.Get("Content-Range"))
		if err != nil {
			return ProbeResult{}, fmt.Errorf("probe %s: %w", url, err)
		}
		res.Size = size
		res.RangesSupported = true
	case http.StatusOK:
		// Server ignored the Range header: no range support.
		res.Size = resp.ContentLength // -1 if unknown
	default:
		return ProbeResult{}, fmt.Errorf("probe %s: unexpected status %s", url, resp.Status)
	}
	return res, nil
}

// parseContentRangeTotal extracts the complete length from a Content-Range
// header such as "bytes 0-0/12345". A total of "*" (unknown length) is an
// error: chunked downloading needs the full size up front.
func parseContentRangeTotal(header string) (int64, error) {
	rest, ok := strings.CutPrefix(header, "bytes ")
	if !ok {
		return 0, fmt.Errorf("malformed Content-Range %q", header)
	}
	_, total, ok := strings.Cut(rest, "/")
	if !ok {
		return 0, fmt.Errorf("malformed Content-Range %q", header)
	}
	size, err := strconv.ParseInt(total, 10, 64)
	if err != nil || size < 0 {
		return 0, fmt.Errorf("unusable Content-Range total %q", header)
	}
	return size, nil
}
