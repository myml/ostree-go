package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func testServer(t *testing.T, dir string, mutate func(*options)) *server {
	t.Helper()
	opts := defaultOptions()
	opts.dir = dir
	if mutate != nil {
		mutate(opts)
	}
	s, err := newServer(opts, nil)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	return s
}

func writeTestFile(t *testing.T, dir, name, content string, mode os.FileMode) string {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
	if err := os.Chmod(full, mode); err != nil {
		t.Fatalf("chmod %s: %v", full, err)
	}
	return full
}

func doRequest(s *server, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestParseArgs(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		opts, err := parseArgs(nil)
		if err != nil {
			t.Fatal(err)
		}
		if opts.dir != "." || opts.port != 0 || opts.portFile != "" ||
			opts.random500sMax != defaultRandomMax || opts.random408sMax != defaultRandomMax {
			t.Fatalf("unexpected defaults: %+v", opts)
		}
	})

	t.Run("long options", func(t *testing.T) {
		opts, err := parseArgs([]string{
			"--autoexit", "--force-range-requests", "--require-basic-auth",
			"--port=18081", "--port-file", "/tmp/port", "--address=127.0.0.1",
			"--random-500s=42", "--random-500s-max=7",
			"--random-408s=3", "--random-408s-max=4",
			"--log-file=/tmp/log", "--expected-cookies=foo=bar",
			"--expected-header=baz=badger", "/srv/tree",
		})
		if err != nil {
			t.Fatal(err)
		}
		if !opts.autoexit || !opts.forceRange || !opts.requireBasicAuth {
			t.Fatalf("boolean options not parsed: %+v", opts)
		}
		if opts.port != 18081 || opts.portFile != "/tmp/port" || opts.address != "127.0.0.1" {
			t.Fatalf("network options not parsed: %+v", opts)
		}
		if opts.random500s != 42 || opts.random500sMax != 7 ||
			opts.random408s != 3 || opts.random408sMax != 4 {
			t.Fatalf("random options not parsed: %+v", opts)
		}
		if opts.logFile != "/tmp/log" || opts.dir != "/srv/tree" {
			t.Fatalf("path options not parsed: %+v", opts)
		}
		if len(opts.expectedCookies) != 1 || opts.expectedCookies[0] != "foo=bar" {
			t.Fatalf("cookies not parsed: %+v", opts.expectedCookies)
		}
		if len(opts.expectedHeaders) != 1 || opts.expectedHeaders[0] != "baz=badger" {
			t.Fatalf("headers not parsed: %+v", opts.expectedHeaders)
		}
	})

	t.Run("short options", func(t *testing.T) {
		opts, err := parseArgs([]string{"-d", "-P18081", "-p", "-", "-P=1234"})
		if err != nil {
			t.Fatal(err)
		}
		if !opts.daemonize || opts.port != 1234 || opts.portFile != "-" {
			t.Fatalf("short options not parsed: %+v", opts)
		}
	})

	t.Run("invalid", func(t *testing.T) {
		cases := [][]string{
			{"--no-such-option"},
			{"--port=notanumber"},
			{"--random-500s=100"},
			{"--random-408s=-1"},
			{"--expected-cookies=novalue"},
			{"a", "b"},
		}
		for _, args := range cases {
			opts, err := parseArgs(args)
			if err == nil {
				err = opts.validate()
			}
			if err == nil {
				t.Errorf("parseArgs(%q) unexpectedly succeeded", args)
			}
		}
	})

	t.Run("daemonize rejects log stdout", func(t *testing.T) {
		opts, err := parseArgs([]string{"--daemonize", "--log-file=-"})
		if err != nil {
			t.Fatal(err)
		}
		if err := opts.validate(); err == nil {
			t.Fatal("expected validation error")
		}
	})
}

func TestServeFile(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "hello.txt", "hello world", 0o644)
	s := testServer(t, dir, nil)

	rec := doRequest(s, http.MethodGet, "/hello.txt", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "hello world" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	sum := sha256.Sum256([]byte("hello world"))
	wantETag := `"` + hex.EncodeToString(sum[:]) + `"`
	if got := rec.Header().Get("ETag"); got != wantETag {
		t.Fatalf("ETag = %q, want %q", got, wantETag)
	}
	if got := rec.Header().Get("Content-Length"); got != "11" {
		t.Fatalf("Content-Length = %q, want 11", got)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges = %q", got)
	}
	if rec.Header().Get("Last-Modified") == "" {
		t.Fatal("Last-Modified missing")
	}

	head := doRequest(s, http.MethodHead, "/hello.txt", nil)
	if head.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d", head.Code)
	}
	if head.Body.Len() != 0 {
		t.Fatalf("HEAD body = %q", head.Body.String())
	}
	if got := head.Header().Get("Content-Length"); got != "11" {
		t.Fatalf("HEAD Content-Length = %q", got)
	}
}

func TestConditionalGet(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "f", "data", 0o644)
	s := testServer(t, dir, nil)

	first := doRequest(s, http.MethodGet, "/f", nil)
	etag := first.Header().Get("ETag")
	lastModified := first.Header().Get("Last-Modified")

	if rec := doRequest(s, http.MethodGet, "/f", map[string]string{"If-None-Match": etag}); rec.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match status = %d, want 304", rec.Code)
	}
	if rec := doRequest(s, http.MethodGet, "/f", map[string]string{"If-None-Match": `"other"`}); rec.Code != http.StatusOK {
		t.Fatalf("mismatched If-None-Match status = %d, want 200", rec.Code)
	}
	if rec := doRequest(s, http.MethodGet, "/f", map[string]string{"If-Modified-Since": lastModified}); rec.Code != http.StatusNotModified {
		t.Fatalf("If-Modified-Since status = %d, want 304", rec.Code)
	}
	past := time.Unix(0, 0).UTC().Format(http.TimeFormat)
	if rec := doRequest(s, http.MethodGet, "/f", map[string]string{"If-Modified-Since": past}); rec.Code != http.StatusOK {
		t.Fatalf("stale If-Modified-Since status = %d, want 200", rec.Code)
	}
}

func TestRangeRequests(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "digits", "0123456789", 0o644)
	s := testServer(t, dir, nil)

	cases := []struct {
		header      string
		wantStatus  int
		wantBody    string
		wantRange   string
		wantLength  string
		noRangeBody bool
	}{
		{"bytes=2-5", 206, "2345", "bytes 2-5/10", "4", false},
		{"bytes=5-", 206, "56789", "bytes 5-9/10", "5", false},
		{"bytes=-3", 206, "789", "bytes 7-9/10", "3", false},
		{"bytes=5-100", 206, "56789", "bytes 5-9/10", "5", false},
		{"bytes=10-", 416, "", "bytes */10", "", true},
		{"bytes=x-y", 200, "0123456789", "", "10", false},
		{"bytes=0-1,3-4", 200, "0123456789", "", "10", false},
	}
	for _, tc := range cases {
		t.Run(tc.header, func(t *testing.T) {
			rec := doRequest(s, http.MethodGet, "/digits", map[string]string{"Range": tc.header})
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if body := rec.Body.String(); body != tc.wantBody {
				t.Fatalf("body = %q, want %q", body, tc.wantBody)
			}
			if got := rec.Header().Get("Content-Range"); got != tc.wantRange {
				t.Fatalf("Content-Range = %q, want %q", got, tc.wantRange)
			}
			if got := rec.Header().Get("Content-Length"); got != tc.wantLength {
				t.Fatalf("Content-Length = %q, want %q", got, tc.wantLength)
			}
		})
	}

	t.Run("head ignores range", func(t *testing.T) {
		rec := doRequest(s, http.MethodHead, "/digits", map[string]string{"Range": "bytes=0-1"})
		if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
			t.Fatalf("HEAD range: status=%d body=%q", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Length"); got != "10" {
			t.Fatalf("HEAD range Content-Length = %q, want 10", got)
		}
	})
}

func TestForceRangeRequests(t *testing.T) {
	dir := t.TempDir()
	content := "0123456789"
	writeTestFile(t, dir, "objects/ab/cdef.file", content, 0o644)
	writeTestFile(t, dir, "other", content, 0o644)
	s := testServer(t, dir, func(o *options) { o.forceRange = true })

	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/objects/ab/cdef.file")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp.ContentLength != int64(len(content)) {
		t.Fatalf("Content-Length = %d, want %d", resp.ContentLength, len(content))
	}
	if len(body) != len(content)/2 {
		t.Fatalf("served %d bytes, want %d (truncated)", len(body), len(content)/2)
	}
	if readErr == nil {
		t.Fatal("expected connection close / unexpected EOF for truncated response")
	}

	// Non-object paths are served in full.
	resp2, err := http.Get(ts.URL + "/other")
	if err != nil {
		t.Fatalf("GET /other: %v", err)
	}
	body2, err := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if err != nil {
		t.Fatalf("reading /other: %v", err)
	}
	if string(body2) != content {
		t.Fatalf("/other body = %q", body2)
	}

	// An explicit Range request is served normally even in force mode.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/objects/ab/cdef.file", nil)
	req.Header.Set("Range", "bytes=0-3")
	resp3, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("range GET: %v", err)
	}
	body3, err := io.ReadAll(resp3.Body)
	resp3.Body.Close()
	if err != nil {
		t.Fatalf("reading range response: %v", err)
	}
	if resp3.StatusCode != http.StatusPartialContent || string(body3) != "0123" {
		t.Fatalf("range status=%d body=%q", resp3.StatusCode, body3)
	}
}

func TestDirectoryListing(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dir, "b.txt", "b", 0o644)
	writeTestFile(t, dir, "a.txt", "a", 0o644)
	writeTestFile(t, dir, "<x>", "x", 0o644)
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := testServer(t, dir, nil)

	rec := doRequest(s, http.MethodGet, "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html" {
		t.Fatalf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"Index of /", "a.txt", "b.txt", "sub", "&lt;x&gt;"} {
		if !strings.Contains(body, want) {
			t.Fatalf("listing missing %q:\n%s", want, body)
		}
	}
	if strings.Index(body, "a.txt") > strings.Index(body, "b.txt") {
		t.Fatalf("listing is not sorted:\n%s", body)
	}
	if strings.Contains(body, "<x>") {
		t.Fatalf("file name was not HTML-escaped:\n%s", body)
	}

	redir := doRequest(s, http.MethodGet, "/sub", nil)
	if redir.Code != http.StatusMovedPermanently || redir.Header().Get("Location") != "/sub/" {
		t.Fatalf("redirect status=%d location=%q", redir.Code, redir.Header().Get("Location"))
	}
	if rec := doRequest(s, http.MethodGet, "/sub/", nil); rec.Code != http.StatusOK {
		t.Fatalf("/sub/ status = %d", rec.Code)
	}

	// index.html takes precedence over the generated listing.
	writeTestFile(t, dir, "sub/index.html", "INDEX", 0o644)
	if rec := doRequest(s, http.MethodGet, "/sub/", nil); rec.Body.String() != "INDEX" {
		t.Fatalf("index.html not served: %q", rec.Body.String())
	}
}

func TestAccessAndErrors(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "private", "secret", 0o600)
	writeTestFile(t, dir, "public", "ok", 0o644)
	if err := os.MkdirAll(filepath.Join(dir, "closed"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dir, "closed/file", "x", 0o644)
	if err := os.Symlink("public", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	s := testServer(t, dir, nil)

	if rec := doRequest(s, http.MethodGet, "/private", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("private file status = %d, want 403", rec.Code)
	}
	if rec := doRequest(s, http.MethodGet, "/closed/", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("closed dir status = %d, want 403", rec.Code)
	}
	if rec := doRequest(s, http.MethodGet, "/link", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("symlink status = %d, want 403", rec.Code)
	}
	if rec := doRequest(s, http.MethodGet, "/missing", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, want 404", rec.Code)
	}
	if rec := doRequest(s, http.MethodPost, "/public", nil); rec.Code != http.StatusNotImplemented {
		t.Fatalf("POST status = %d, want 501", rec.Code)
	}
	if rec := doRequest(s, http.MethodGet, "/../etc/passwd", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("traversal status = %d, want 403", rec.Code)
	}
}

func TestExpectedCookiesAndHeaders(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "f", "x", 0o644)
	s := testServer(t, dir, func(o *options) {
		o.expectedCookies = []string{"foo=bar", "baz=badger"}
		o.expectedHeaders = []string{"X-Test=yes"}
	})

	if rec := doRequest(s, http.MethodGet, "/f", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("missing cookie status = %d, want 403", rec.Code)
	}
	if rec := doRequest(s, http.MethodGet, "/f", map[string]string{
		"Cookie": "foo=bar", "X-Test": "yes",
	}); rec.Code != http.StatusForbidden {
		t.Fatalf("partial cookie status = %d, want 403", rec.Code)
	}
	if rec := doRequest(s, http.MethodGet, "/f", map[string]string{
		"Cookie": "foo=bar; baz=badger", "X-Test": "yes",
	}); rec.Code != http.StatusOK {
		t.Fatalf("valid cookie/header status = %d, want 200", rec.Code)
	}
	if rec := doRequest(s, http.MethodGet, "/f", map[string]string{
		"Cookie": "foo=bar; baz=badger", "X-Test": "no",
	}); rec.Code != http.StatusForbidden {
		t.Fatalf("bad header status = %d, want 403", rec.Code)
	}
}

func TestBasicAuth(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "f", "x", 0o644)
	s := testServer(t, dir, func(o *options) { o.requireBasicAuth = true })

	rec := doRequest(s, http.MethodGet, "/f", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, `realm="auth-test"`) {
		t.Fatalf("WWW-Authenticate = %q", got)
	}

	req := httptest.NewRequest(http.MethodGet, "/f", nil)
	req.SetBasicAuth("foouser", "wrong")
	bad := httptest.NewRecorder()
	s.ServeHTTP(bad, req)
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("bad auth status = %d, want 401", bad.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/f", nil)
	req.SetBasicAuth("foouser", "barpw")
	good := httptest.NewRecorder()
	s.ServeHTTP(good, req)
	if good.Code != http.StatusOK {
		t.Fatalf("good auth status = %d, want 200", good.Code)
	}
}

func TestRandomFailures(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "f", "x", 0o644)
	s := testServer(t, dir, func(o *options) {
		o.random500s = 100
		o.random500sMax = 2
		o.random408s = 0
	})
	for i := 0; i < 2; i++ {
		if rec := doRequest(s, http.MethodGet, "/f", nil); rec.Code != http.StatusInternalServerError {
			t.Fatalf("request %d status = %d, want 500", i, rec.Code)
		}
	}
	if rec := doRequest(s, http.MethodGet, "/f", nil); rec.Code != http.StatusOK {
		t.Fatalf("after 500 limit status = %d, want 200", rec.Code)
	}

	s408 := testServer(t, dir, func(o *options) {
		o.random408s = 100
		o.random408sMax = 1
	})
	if rec := doRequest(s408, http.MethodGet, "/f", nil); rec.Code != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want 408", rec.Code)
	}
	if rec := doRequest(s408, http.MethodGet, "/f", nil); rec.Code != http.StatusOK {
		t.Fatalf("after 408 limit status = %d, want 200", rec.Code)
	}
}

func TestParseRange(t *testing.T) {
	cases := []struct {
		header   string
		size     int64
		start    int64
		end      int64
		hasRange bool
		invalid  bool
	}{
		{"", 10, 0, 0, false, false},
		{"bytes=0-0", 10, 0, 0, true, false},
		{"bytes=2-5", 10, 2, 5, true, false},
		{"bytes=5-", 10, 5, 9, true, false},
		{"bytes=-4", 10, 6, 9, true, false},
		{"bytes=-20", 10, 0, 9, true, false},
		{"bytes=10-", 10, 0, 0, false, true},
		{"bytes=-0", 10, 0, 0, false, true},
		{"bytes=0-1,2-3", 10, 0, 0, false, false},
		{"bytes=abc", 10, 0, 0, false, false},
		{"bytes=5-2", 10, 0, 0, false, false},
	}
	for _, tc := range cases {
		start, end, hasRange, invalid := parseRange(tc.header, tc.size)
		if start != tc.start || end != tc.end || hasRange != tc.hasRange || invalid != tc.invalid {
			t.Errorf("parseRange(%q) = (%d,%d,%v,%v), want (%d,%d,%v,%v)",
				tc.header, start, end, hasRange, invalid,
				tc.start, tc.end, tc.hasRange, tc.invalid)
		}
	}
}

func TestWritePortFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "port")
	if err := writePortFile(path, 12345); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "12345\n" {
		t.Fatalf("port file = %q", data)
	}
}

func TestAutoexit(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: testServer(t, dir, nil)}
	var once sync.Once
	go watchRoot(dir, srv, &once)

	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil && err != http.ErrServerClosed {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		srv.Close()
		t.Fatal("server did not exit after root directory was removed")
	}
}

// TestDaemonizeEndToEnd builds the command and exercises the exact invocation
// used by tests/libtest.sh: --daemonize --autoexit -p PORTFILE DIR.
func TestDaemonizeEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in -short mode")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go command not available")
	}
	bin := filepath.Join(t.TempDir(), "testhttpd")
	build := exec.Command(goBin, "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building testhttpd: %v\n%s", err, out)
	}

	dir := t.TempDir()
	writeTestFile(t, dir, "hello", "world", 0o644)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	portFile := filepath.Join(t.TempDir(), "port")

	cmd := exec.Command(bin, "--autoexit", "--daemonize", "-p", portFile, dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("daemonize: %v\n%s", err, out)
	}
	portData, err := os.ReadFile(portFile)
	if err != nil {
		t.Fatalf("port file after daemonize: %v", err)
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(portData)))
	if err != nil {
		t.Fatalf("port %q: %v", portData, err)
	}
	addr := fmt.Sprintf("http://127.0.0.1:%d/hello", port)

	resp, err := http.Get(addr)
	if err != nil {
		t.Fatalf("GET %s: %v", addr, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "world" {
		t.Fatalf("body = %q", body)
	}

	// Removing the served directory must make the daemon exit.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(addr)
		if err != nil {
			return
		}
		resp.Body.Close()
		if time.Now().After(deadline) {
			t.Fatal("daemon did not exit after root directory was removed")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestLogFile(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "f", "x", 0o644)
	logPath := filepath.Join(t.TempDir(), "httpd.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s, err := newServer(func() *options {
		o := defaultOptions()
		o.dir = dir
		return o
	}(), f)
	if err != nil {
		t.Fatal(err)
	}
	if rec := doRequest(s, http.MethodGet, "/f", nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("serving /f")) || !bytes.Contains(data, []byte("status: OK (200)")) {
		t.Fatalf("log contents:\n%s", data)
	}
}
