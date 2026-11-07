package pull

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/myml/ostree-go/pkg/repo"
)

// fs404 is the sentinel returned for HTTP 404/410 and missing local files.
var fs404 = errors.New("not found")

// defaultUserAgent mirrors OSTREE_FETCHER_USERAGENT_STRING (PACKAGE_NAME
// "/" PACKAGE_VERSION).  --append-user-agent appends to it.
const defaultUserAgent = "libostree/2025.2"

// defaultNetworkRetries matches DEFAULT_N_NETWORK_RETRIES in
// ostree-repo-pull.c.  The option counts retries *after* the first attempt.
const defaultNetworkRetries = 5

// httpHeader is one extra header sent with every request.
type httpHeader struct {
	Name  string
	Value string
}

// pullStats tracks the counters printed by the pull status line.
type pullStats struct {
	metadataFetched int
	contentFetched  int
	metadataLocal   int
	contentLocal    int
}

// fetcher performs object/metadata downloads.  A single fetcher can carry
// distinct mirror lists for metadata ("url"/mirrorlist) and content
// ("contenturl"), as libostree's pull does.
type fetcher struct {
	metaMirrors    []string
	contentMirrors []string
	local          bool

	client    *http.Client
	headers   []httpHeader
	userAgent string
	cookies   []cookie
	retries   int

	// dest is the repository being pulled into; caches are local repositories
	// consulted before the network (--localcache-repo).
	dest     *repo.Repo
	caches   []*repo.Repo
	cacheDir string
	dryRun   bool
	stats    *pullStats
}

// newFetcher builds a fetcher for a single base URL (file:// path or
// http(s) URL).  It is kept for callers that do not need remote options.
func newFetcher(raw string) (*fetcher, error) {
	f := newBaseFetcher()
	if err := f.setMetaMirrors([]string{raw}); err != nil {
		return nil, err
	}
	if err := f.setContentMirrors([]string{raw}); err != nil {
		return nil, err
	}
	return f, nil
}

func newBaseFetcher() *fetcher {
	return &fetcher{
		client:    &http.Client{Timeout: 120 * time.Second},
		userAgent: defaultUserAgent,
		retries:   defaultNetworkRetries,
		stats:     &pullStats{},
	}
}

func (f *fetcher) setMetaMirrors(specs []string) error {
	mirrors, local, err := normalizeMirrors(specs)
	if err != nil {
		return err
	}
	f.metaMirrors = mirrors
	f.local = local
	return nil
}

func (f *fetcher) setContentMirrors(specs []string) error {
	mirrors, _, err := normalizeMirrors(specs)
	if err != nil {
		return err
	}
	f.contentMirrors = mirrors
	return nil
}

// normalizeMirrors converts configured base URLs into the internal form:
// filesystem paths for local repositories and scheme-trimmed http(s) URLs.
func normalizeMirrors(specs []string) (mirrors []string, local bool, err error) {
	for _, spec := range specs {
		m, isLocal, err := normalizeMirror(spec)
		if err != nil {
			return nil, false, err
		}
		mirrors = append(mirrors, m)
		if isLocal {
			local = true
		}
	}
	return mirrors, local, nil
}

func normalizeMirror(spec string) (string, bool, error) {
	if spec == "" {
		return "", false, fmt.Errorf("Unsupported URL: %s", spec)
	}
	if strings.HasPrefix(spec, "file://") {
		u, err := url.Parse(spec)
		if err != nil {
			return "", false, err
		}
		return u.Path, true, nil
	}
	if !strings.Contains(spec, "://") {
		return spec, true, nil
	}
	if !strings.HasPrefix(spec, "http://") && !strings.HasPrefix(spec, "https://") {
		// Matches _ostree_fetcher_uri_new()'s error text.
		return "", false, fmt.Errorf("Invalid URI scheme in %s", spec)
	}
	return strings.TrimSuffix(spec, "/"), false, nil
}

// localBase returns the filesystem base for a local remote, or "".
func (f *fetcher) localBase() string {
	for _, m := range f.metaMirrors {
		if _, local, err := normalizeMirror(m); err == nil && local {
			return m
		}
	}
	return ""
}

func joinURL(base, rel string) string {
	if rel == "" {
		return base
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(rel, "/")
}

// get fetches rel from the metadata mirror list.
func (f *fetcher) get(rel string) ([]byte, error) {
	return f.getFromMirrors(f.metaMirrors, rel)
}

// getContent fetches rel from the content mirror list, falling back to the
// metadata mirrors when no content URL was configured.
func (f *fetcher) getContent(rel string) ([]byte, error) {
	mirrors := f.contentMirrors
	if len(mirrors) == 0 {
		mirrors = f.metaMirrors
	}
	return f.getFromMirrors(mirrors, rel)
}

func (f *fetcher) getFromMirrors(mirrors []string, rel string) ([]byte, error) {
	if len(mirrors) == 0 {
		return nil, fmt.Errorf("No mirrors are configured")
	}
	var lastErr error
	for _, base := range mirrors {
		data, err := f.getOne(base, rel)
		if err == nil {
			return data, nil
		}
		lastErr = err
		// 404 is per-mirror: the object may exist on a later mirror.
	}
	return nil, lastErr
}

func (f *fetcher) getOne(base, rel string) ([]byte, error) {
	basePath, local, err := normalizeMirror(base)
	if err != nil {
		return nil, err
	}
	if local {
		data, err := os.ReadFile(filepath.Join(basePath, filepath.FromSlash(rel)))
		if err != nil {
			if os.IsNotExist(err) {
				return nil, fs404
			}
			return nil, err
		}
		return data, nil
	}
	return f.getURL(joinURL(basePath, rel))
}

// fetchAbsolute retrieves a fully-qualified URL (mirrorlist, metalink, or the
// config file used to sanity-check mirror candidates).
func (f *fetcher) fetchAbsolute(rawURL string) ([]byte, error) {
	if strings.HasPrefix(rawURL, "file://") || !strings.Contains(rawURL, "://") {
		basePath, local, err := normalizeMirror(rawURL)
		if err != nil {
			return nil, err
		}
		if !local {
			return nil, fmt.Errorf("Unsupported URL: %s", rawURL)
		}
		data, err := os.ReadFile(basePath)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, fs404
			}
			return nil, err
		}
		return data, nil
	}
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return nil, fmt.Errorf("Invalid URI scheme in %s", rawURL)
	}
	return f.getURL(rawURL)
}

func (f *fetcher) applyHeaders(req *http.Request) {
	req.Header.Set("User-Agent", f.userAgent)
	for _, h := range f.headers {
		if h.Name == "" {
			continue
		}
		req.Header.Add(h.Name, h.Value)
	}
	if len(f.cookies) > 0 {
		if value := cookieHeader(f.cookies, req.URL); value != "" {
			req.Header.Set("Cookie", value)
		}
	}
}

func (f *fetcher) attempts() int {
	n := f.retries + 1
	if n < 1 {
		n = 1
	}
	return n
}

// getURL downloads rawURL, resuming partial transfers with Range/If-Range
// requests.  Transient failures (network errors, 408 and 5xx) are retried
// according to --network-retries.
func (f *fetcher) getURL(rawURL string) ([]byte, error) {
	var data []byte
	var etag string
	var lastErr error
	partial := false

	for attempt := 0; attempt < f.attempts(); attempt++ {
		req, err := http.NewRequest("GET", rawURL, nil)
		if err != nil {
			return nil, err
		}
		f.applyHeaders(req)
		if len(data) > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", len(data)))
			if etag != "" {
				req.Header.Set("If-Range", etag)
			}
		}

		resp, err := f.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if etag == "" {
			etag = resp.Header.Get("ETag")
		}
		status := resp.StatusCode
		switch {
		case status == http.StatusNotFound || status == http.StatusGone:
			drainBody(resp)
			return nil, fs404
		case status == http.StatusNotModified:
			drainBody(resp)
			return data, nil
		case status == http.StatusPartialContent:
			body, readErr := readBody(resp)
			start, total, hasRange := parseContentRange(resp.Header.Get("Content-Range"))
			switch {
			case hasRange && start == int64(len(data)):
				data = append(data, body...)
			case hasRange && start == 0:
				data = body
			default:
				// Unexpected range; discard and restart from scratch.
				data = body
				lastErr = fmt.Errorf("While fetching %s: unexpected Content-Range", rawURL)
				partial = true
				continue
			}
			partial = readErr != nil || (hasRange && total >= 0 && int64(len(data)) < total)
			if !partial {
				return data, nil
			}
			lastErr = partialError(rawURL)
			continue
		case status == http.StatusOK:
			body, readErr := readBody(resp)
			if len(data) > 0 {
				// The server ignored Range; restart with the full body.
				data = nil
			}
			data = append(data, body...)
			expected := resp.ContentLength
			partial = readErr != nil || (expected >= 0 && int64(len(data)) < expected)
			if !partial {
				return data, nil
			}
			lastErr = partialError(rawURL)
			continue
		case status == http.StatusRequestTimeout || status >= 500:
			drainBody(resp)
			lastErr = fmt.Errorf("HTTP %d %s", status, http.StatusText(status))
			continue
		default:
			drainBody(resp)
			return nil, fmt.Errorf("HTTP %d %s for %s", status, http.StatusText(status), rawURL)
		}
	}

	if partial {
		if lastErr == nil {
			lastErr = partialError(rawURL)
		}
		return nil, lastErr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("While fetching %s: Download incomplete", rawURL)
	}
	return nil, lastErr
}

func partialError(rawURL string) error {
	return fmt.Errorf("While fetching %s: Transferred a partial file", rawURL)
}

func readBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return body, err
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return body, err
	}
	return body, nil
}

func drainBody(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// parseContentRange parses "bytes START-END/TOTAL" (TOTAL may be "*").
func parseContentRange(value string) (start, total int64, ok bool) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "bytes ") {
		return 0, -1, false
	}
	spec := strings.TrimSpace(strings.TrimPrefix(value, "bytes "))
	spec = strings.TrimSpace(spec)
	slash := strings.LastIndexByte(spec, '/')
	if slash < 0 {
		return 0, -1, false
	}
	rangePart, totalPart := spec[:slash], spec[slash+1:]
	first, _, hasDash := strings.Cut(rangePart, "-")
	if !hasDash {
		return 0, -1, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(first), 10, 64)
	if err != nil {
		return 0, -1, false
	}
	total = -1
	if totalPart != "*" {
		if v, err := strconv.ParseInt(strings.TrimSpace(totalPart), 10, 64); err == nil {
			total = v
		}
	}
	return n, total, true
}
