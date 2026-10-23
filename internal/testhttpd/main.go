// Command testhttpd is a pure Go replacement for the C
// `ostree-trivial-httpd` helper used by OSTree's shell tests.
//
// It implements the behavior required by tests/libtest.sh and the tests that
// run the helper directly: static file serving, directory listings, ETag /
// Last-Modified validation, range requests, forced half-file responses (to
// exercise pull resume), optional basic auth, expected cookies/headers,
// random 500/408 responses, daemonizing and auto-exit when the served
// directory disappears.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// daemonEnv marks the re-exec'ed child after --daemonize.
	daemonEnv = "OSTREE_TESTHTTPD_DAEMONIZED"
	// statusFDEnv is the file descriptor used by the child to report setup
	// success (one byte '0') or failure (one byte '1') to the parent.
	statusFDEnv = "OSTREE_TESTHTTPD_STATUS_FD"

	defaultRandomMax = 100
	serverHeader     = "ostree-httpd "
	autoexitInterval = 100 * time.Millisecond
)

type options struct {
	daemonize        bool
	autoexit         bool
	port             int
	portFile         string
	address          string
	forceRange       bool
	requireBasicAuth bool
	random500s       int
	random500sMax    int
	random408s       int
	random408sMax    int
	logFile          string
	expectedCookies  []string
	expectedHeaders  []string
	dir              string
	showHelp         bool
	showVersion      bool
}

func defaultOptions() *options {
	return &options{
		random500sMax: defaultRandomMax,
		random408sMax: defaultRandomMax,
		dir:           ".",
	}
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `Usage:
  testhttpd [OPTION?] [DIR] - Simple webserver

Help Options:
  -h, --help                        Show help options

Application Options:
  -d, --daemonize                   Fork into background when ready
      --autoexit                    Automatically exit when directory is deleted
  -P, --port=PORT                   Use the specified TCP port
  -p, --port-file=PATH              Write port number to PATH (- for standard output)
      --address=ADDRESS             Bind to ADDRESS (default: all interfaces)
      --force-range-requests        Force range requests by only serving half of files
      --require-basic-auth          Require username foouser, password barpw
      --random-500s=PERCENTAGE      Generate random HTTP 500 errors approximately for PERCENTAGE requests
      --random-500s-max=MAX         Limit HTTP 500 errors to MAX (default 100)
      --random-408s=PERCENTAGE      Generate random HTTP 408 errors approximately for PERCENTAGE requests
      --random-408s-max=MAX         Limit HTTP 408 errors to MAX (default 100)
      --log-file=PATH               Put logs here (use - for stdout)
      --expected-cookies=KEY=VALUE  Expect given cookies in the http request
      --expected-header=KEY=VALUE   Expect given headers in the http request
      --version                     Print version information and exit
`)
}

func parseArgs(args []string) (*options, error) {
	opts := defaultOptions()
	var positional []string

	nextValue := func(i *int, name, inline string, hasInline bool) (string, error) {
		if hasInline {
			return inline, nil
		}
		if *i+1 >= len(args) {
			return "", fmt.Errorf("Option %s requires an argument", name)
		}
		*i++
		return args[*i], nil
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			positional = append(positional, args[i+1:]...)
			i = len(args)
		case arg == "-h" || arg == "--help":
			opts.showHelp = true
		case arg == "--version":
			opts.showVersion = true
		case arg == "-d" || arg == "--daemonize":
			opts.daemonize = true
		case arg == "--autoexit":
			opts.autoexit = true
		case arg == "--force-range-requests":
			opts.forceRange = true
		case arg == "--require-basic-auth":
			opts.requireBasicAuth = true
		case strings.HasPrefix(arg, "--"):
			name, inline, hasInline := strings.Cut(arg[2:], "=")
			var (
				value string
				err   error
			)
			switch name {
			case "port", "port-file", "address", "log-file", "random-500s",
				"random-500s-max", "random-408s", "random-408s-max",
				"expected-cookies", "expected-header":
				value, err = nextValue(&i, "--"+name, inline, hasInline)
			default:
				return nil, fmt.Errorf("Unknown option --%s", name)
			}
			if err != nil {
				return nil, err
			}
			if err := applyValueOption(opts, name, value); err != nil {
				return nil, err
			}
		case strings.HasPrefix(arg, "-") && arg != "-":
			// Short options. -P and -p consume a value, -d is a flag.
			// GOption accepts both "-P 80" and "-P80", plus "-P=80".
			short := arg[1:]
			for len(short) > 0 {
				switch short[0] {
				case 'd':
					opts.daemonize = true
					short = short[1:]
				case 'P', 'p':
					name := "port"
					if short[0] == 'p' {
						name = "port-file"
					}
					rest := short[1:]
					if strings.HasPrefix(rest, "=") {
						rest = rest[1:]
					}
					var value string
					var err error
					if rest != "" {
						value = rest
					} else {
						value, err = nextValue(&i, "-"+string(short[0]), "", false)
						if err != nil {
							return nil, err
						}
					}
					if err := applyValueOption(opts, name, value); err != nil {
						return nil, err
					}
					short = ""
				default:
					return nil, fmt.Errorf("Unknown option -%c", short[0])
				}
			}
		default:
			positional = append(positional, arg)
		}
	}

	switch len(positional) {
	case 0:
		// keep default "."
	case 1:
		opts.dir = positional[0]
	default:
		return nil, fmt.Errorf("Too many arguments given")
	}
	return opts, nil
}

func applyValueOption(opts *options, name, value string) error {
	parseInt := func(label string) (int, error) {
		n, err := strconv.Atoi(value)
		if err != nil {
			return 0, fmt.Errorf("Cannot parse integer value '%s' for %s", value, label)
		}
		return n, nil
	}
	var err error
	switch name {
	case "port":
		opts.port, err = parseInt("--port")
	case "port-file":
		opts.portFile = value
	case "address":
		opts.address = value
	case "log-file":
		opts.logFile = value
	case "random-500s":
		opts.random500s, err = parseInt("--random-500s")
	case "random-500s-max":
		opts.random500sMax, err = parseInt("--random-500s-max")
	case "random-408s":
		opts.random408s, err = parseInt("--random-408s")
	case "random-408s-max":
		opts.random408sMax, err = parseInt("--random-408s-max")
	case "expected-cookies":
		if !strings.Contains(value, "=") {
			return fmt.Errorf("Invalid --expected-cookies value '%s' (expected KEY=VALUE)", value)
		}
		opts.expectedCookies = append(opts.expectedCookies, value)
	case "expected-header":
		if !strings.Contains(value, "=") {
			return fmt.Errorf("Invalid --expected-header value '%s' (expected KEY=VALUE)", value)
		}
		opts.expectedHeaders = append(opts.expectedHeaders, value)
	default:
		return fmt.Errorf("Unknown option --%s", name)
	}
	return err
}

func (o *options) validate() error {
	if o.random500s < 0 || o.random500s > 99 {
		return fmt.Errorf("Invalid --random-500s=%d", o.random500s)
	}
	if o.random408s < 0 || o.random408s > 99 {
		return fmt.Errorf("Invalid --random-408s=%d", o.random408s)
	}
	if o.random500sMax < 0 {
		return fmt.Errorf("Invalid --random-500s-max=%d", o.random500sMax)
	}
	if o.random408sMax < 0 {
		return fmt.Errorf("Invalid --random-408s-max=%d", o.random408sMax)
	}
	if o.daemonize && o.logFile == "-" {
		return fmt.Errorf("Cannot use --log-file=- and --daemonize at the same time")
	}
	if o.port < 0 || o.port > 65535 {
		return fmt.Errorf("Invalid --port=%d", o.port)
	}
	return nil
}

// logger serializes writes to the optional log file. The C implementation
// writes a timestamp prefix on every call; tests do not parse it, but keep
// the format close for debugging.
type logger struct {
	mu sync.Mutex
	w  io.Writer
}

func newLogger(w io.Writer) *logger {
	if w == nil {
		return nil
	}
	return &logger{w: w}
}

func (l *logger) logf(format string, args ...any) {
	if l == nil || l.w == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	fmt.Fprintf(l.w, "%s.%06d - %s", now.Format("2006-01-02 15:04:05"),
		now.Nanosecond()/1000, fmt.Sprintf(format, args...))
}

type server struct {
	opts *options
	root string

	log *logger

	mu     sync.Mutex
	rng    *rand.Rand
	n500   int
	n408   int
	closed bool
}

func newServer(opts *options, logw io.Writer) (*server, error) {
	root, err := filepath.Abs(opts.dir)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", opts.dir, err)
	}
	fi, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", root, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("opening %s: not a directory", root)
	}
	return &server{
		opts: opts,
		root: root,
		log:  newLogger(logw),
		rng:  rand.New(rand.NewSource(time.Now().UnixNano())),
	}, nil
}

func (s *server) logf(format string, args ...any) {
	if s.log != nil {
		s.log.logf(format, args...)
	}
}

// ServeHTTP implements the request handling of ostree-trivial-httpd.
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Server", serverHeader)

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusNotImplemented)
		s.logf("%s %s\n  status: Not Implemented (501)\n", r.Method, r.URL.Path)
		return
	}

	if !s.checkExpectedCookies(w, r) {
		s.logf("%s %s\n  status: Forbidden (403) [expected cookie missing]\n", r.Method, r.URL.Path)
		return
	}
	if !s.checkExpectedHeaders(w, r) {
		s.logf("%s %s\n  status: Forbidden (403) [expected header missing]\n", r.Method, r.URL.Path)
		return
	}
	if s.opts.requireBasicAuth && !s.checkBasicAuth(w, r) {
		s.logf("%s %s\n  status: Unauthorized (401)\n", r.Method, r.URL.Path)
		return
	}

	s.logf("serving %s\n", r.URL.Path)

	if strings.Contains(r.URL.Path, "../") {
		w.WriteHeader(http.StatusForbidden)
		s.logf("  status: Forbidden (403)\n")
		return
	}

	if status := s.maybeRandomFailure(); status != 0 {
		w.WriteHeader(status)
		s.logf("  status: %s (%d)\n", http.StatusText(status), status)
		return
	}

	s.servePath(w, r, r.URL.Path)
}

func (s *server) maybeRandomFailure() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.opts.random500s > 0 && s.n500 < s.opts.random500sMax &&
		s.rng.Intn(100) < s.opts.random500s {
		s.n500++
		return http.StatusInternalServerError
	}
	if s.opts.random408s > 0 && s.n408 < s.opts.random408sMax &&
		s.rng.Intn(100) < s.opts.random408s {
		s.n408++
		return http.StatusRequestTimeout
	}
	return 0
}

func (s *server) checkExpectedCookies(w http.ResponseWriter, r *http.Request) bool {
	for _, expected := range s.opts.expectedCookies {
		name, value, _ := strings.Cut(expected, "=")
		c, err := r.Cookie(name)
		if err != nil || c.Value != value {
			s.logf("Expected cookie not found %s\n", expected)
			w.WriteHeader(http.StatusForbidden)
			return false
		}
	}
	return true
}

func (s *server) checkExpectedHeaders(w http.ResponseWriter, r *http.Request) bool {
	for _, expected := range s.opts.expectedHeaders {
		name, value, _ := strings.Cut(expected, "=")
		found := r.Header.Get(name)
		if found == "" {
			s.logf("Expected header not found %s\n", name)
			w.WriteHeader(http.StatusForbidden)
			return false
		}
		if found != value {
			s.logf("Expected header %s: %s but found %s\n", name, value, found)
			w.WriteHeader(http.StatusForbidden)
			return false
		}
	}
	return true
}

func (s *server) checkBasicAuth(w http.ResponseWriter, r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	if !ok || user != "foouser" || pass != "barpw" {
		w.Header().Set("WWW-Authenticate", `Basic realm="auth-test"`)
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	return true
}

// resolvePath maps a URL path to a filesystem path, rejecting traversal.
func (s *server) resolvePath(urlPath string) (string, bool) {
	if strings.ContainsRune(urlPath, 0) {
		return "", false
	}
	cleaned := strings.TrimPrefix(urlPath, "/")
	for _, segment := range strings.Split(cleaned, "/") {
		if segment == ".." {
			return "", false
		}
	}
	if cleaned == "" {
		return s.root, true
	}
	return filepath.Join(s.root, filepath.FromSlash(cleaned)), true
}

func (s *server) servePath(w http.ResponseWriter, r *http.Request, urlPath string) {
	full, ok := s.resolvePath(urlPath)
	if !ok {
		w.WriteHeader(http.StatusForbidden)
		s.logf("  status: Forbidden (403)\n")
		return
	}

	fi, err := os.Lstat(full)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, fs.ErrNotExist):
			status = http.StatusNotFound
		case errors.Is(err, fs.ErrPermission):
			status = http.StatusForbidden
		}
		w.WriteHeader(status)
		s.logf("  status: %s (%d)\n", http.StatusText(status), status)
		return
	}

	if !safeToAccess(fi) {
		w.WriteHeader(http.StatusForbidden)
		s.logf("  status: Forbidden (403)\n")
		return
	}

	if fi.IsDir() {
		s.serveDirectory(w, r, urlPath, full)
		return
	}
	if !fi.Mode().IsRegular() {
		w.WriteHeader(http.StatusForbidden)
		s.logf("  status: Forbidden (403)\n")
		return
	}
	s.serveFile(w, r, urlPath, full, fi)
}

// safeToAccess mirrors is_safe_to_access() from the C helper: only regular
// files and directories, and the object must be world-readable (and, for
// directories, world-executable).
func safeToAccess(fi fs.FileInfo) bool {
	mode := fi.Mode()
	if mode&fs.ModeSymlink != 0 {
		return false
	}
	if !mode.IsRegular() && !mode.IsDir() {
		return false
	}
	perm := mode.Perm()
	if perm&0o004 == 0 {
		return false
	}
	if mode.IsDir() && perm&0o001 == 0 {
		return false
	}
	return true
}

func (s *server) serveDirectory(w http.ResponseWriter, r *http.Request, urlPath, full string) {
	// Redirect /dir to /dir/ so that relative links work.
	if !strings.HasSuffix(urlPath, "/") {
		w.Header().Set("Location", urlPath+"/")
		w.WriteHeader(http.StatusMovedPermanently)
		s.logf("  status: Moved Permanently (301)\n")
		return
	}

	indexPath := filepath.Join(full, "index.html")
	if fi, err := os.Lstat(indexPath); err == nil && safeToAccess(fi) && fi.Mode().IsRegular() {
		s.servePath(w, r, path.Join(urlPath, "index.html"))
		return
	}

	entries, err := os.ReadDir(full)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		s.logf("  status: Internal Server Error (500)\n")
		return
	}

	escapedPath := html.EscapeString(urlPath)
	var b strings.Builder
	b.WriteString("<html>\r\n")
	fmt.Fprintf(&b, "<head><title>Index of %s</title></head>\r\n", escapedPath)
	fmt.Fprintf(&b, "<body><h1>Index of %s</h1>\r\n<p>\r\n", escapedPath)
	for _, entry := range entries {
		name := html.EscapeString(entry.Name())
		fmt.Fprintf(&b, "<a href=\"%s\">%s</a><br>\r\n", name, name)
	}
	b.WriteString("</body>\r\n</html>\r\n")

	body := b.String()
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = io.WriteString(w, body)
	}
	s.logf("  status: OK (200)\n")
}

func (s *server) serveFile(w http.ResponseWriter, r *http.Request, urlPath, full string, fi fs.FileInfo) {
	data, err := os.ReadFile(full)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		s.logf("  status: Internal Server Error (500)\n")
		return
	}
	size := int64(len(data))

	sum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	lastModified := fi.ModTime().UTC()
	lastModifiedHeader := lastModified.Format(http.TimeFormat)

	w.Header().Set("ETag", etag)
	w.Header().Set("Last-Modified", lastModifiedHeader)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", "application/octet-stream")

	// Caching validators, in the same precedence order as the C helper.
	if ifNoneMatch := r.Header.Get("If-None-Match"); ifNoneMatch != "" {
		if ifNoneMatch == etag {
			w.WriteHeader(http.StatusNotModified)
			s.logf("  status: Not Modified (304)\n")
			return
		}
	} else if ifModifiedSince := r.Header.Get("If-Modified-Since"); ifModifiedSince != "" {
		if t, err := http.ParseTime(ifModifiedSince); err == nil &&
			lastModified.Unix() <= t.Unix() {
			w.WriteHeader(http.StatusNotModified)
			s.logf("  status: Not Modified (304)\n")
			return
		}
	}

	// The C helper only processes Range for GET; HEAD always reports the full
	// object length. Mirror that.
	start, end, hasRange, invalidRange := int64(0), int64(0), false, false
	if r.Method == http.MethodGet {
		start, end, hasRange, invalidRange = parseRange(r.Header.Get("Range"), size)
	}
	if invalidRange {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		s.logf("  status: Requested Range Not Satisfiable (416)\n")
		return
	}

	// Force-range mode: serve only half of an object while declaring the full
	// length, then close the connection. This makes pull clients retry with a
	// Range request.
	if s.opts.forceRange && !hasRange && r.Method == http.MethodGet &&
		strings.Contains(urlPath, "/objects") {
		s.serveTruncated(w, data)
		return
	}

	if hasRange {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		if r.Method == http.MethodGet {
			_, _ = w.Write(data[start : end+1])
		}
		s.logf("  status: Partial Content (206)\n")
		return
	}

	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
	s.logf("  status: OK (200)\n")
}

// serveTruncated hijacks the connection so that it can declare Content-Length
// for the full object but write only half of it, then close. net/http would
// otherwise report a short write, so a raw response is required here.
func (s *server) serveTruncated(w http.ResponseWriter, data []byte) {
	size := int64(len(data))
	half := size / 2

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		// Fallback (e.g. an in-process recorder): just write half; callers
		// that need the truncated-socket behavior use a real server.
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data[:half])
		return
	}
	conn, buf, err := hijacker.Hijack()
	if err != nil {
		s.logf("hijack failed: %v\n", err)
		return
	}
	defer conn.Close()

	header := w.Header()
	fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\n")
	for _, name := range []string{"Server", "ETag", "Last-Modified", "Content-Type"} {
		if value := header.Get(name); value != "" {
			fmt.Fprintf(buf, "%s: %s\r\n", name, value)
		}
	}
	fmt.Fprintf(buf, "Content-Length: %d\r\n", size)
	fmt.Fprintf(buf, "Connection: close\r\n\r\n")
	if half > 0 {
		_, _ = buf.Write(data[:half])
	}
	_ = buf.Flush()
	s.logf("  status: OK (200) [forced range, served %d/%d bytes]\n", half, size)
}

// parseRange parses a single byte range. Multiple ranges are treated as
// absent (the full object is served); malformed ranges are ignored, and
// syntactically valid but unsatisfiable ranges produce invalidRange=true.
func parseRange(header string, size int64) (start, end int64, hasRange, invalidRange bool) {
	header = strings.TrimSpace(header)
	if header == "" || !strings.HasPrefix(header, "bytes=") {
		return 0, 0, false, false
	}
	spec := strings.TrimSpace(strings.TrimPrefix(header, "bytes="))
	if strings.Contains(spec, ",") {
		return 0, 0, false, false
	}
	first, last, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, false, false
	}
	first = strings.TrimSpace(first)
	last = strings.TrimSpace(last)
	if first == "" {
		// Suffix range: bytes=-N means the last N bytes.
		if last == "" {
			return 0, 0, false, false
		}
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n < 0 {
			return 0, 0, false, false
		}
		if n == 0 || size == 0 {
			return 0, 0, false, true
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true, false
	}
	from, err := strconv.ParseInt(first, 10, 64)
	if err != nil || from < 0 {
		return 0, 0, false, false
	}
	if last == "" {
		if from >= size {
			return 0, 0, false, true
		}
		return from, size - 1, true, false
	}
	to, err := strconv.ParseInt(last, 10, 64)
	if err != nil || to < from {
		return 0, 0, false, false
	}
	if from >= size {
		return 0, 0, false, true
	}
	if to >= size {
		to = size - 1
	}
	return from, to, true, false
}

// writePortFile writes the bound port, matching the C helper's "PORT\n"
// format. A path of "-" means standard output.
func writePortFile(path string, port int) error {
	value := strconv.Itoa(port) + "\n"
	if path == "-" {
		_, err := io.WriteString(os.Stdout, value)
		return err
	}
	return os.WriteFile(path, []byte(value), 0o666)
}

func notifyStatus(code byte) {
	fdStr := os.Getenv(statusFDEnv)
	if fdStr == "" {
		return
	}
	fd, err := strconv.Atoi(fdStr)
	if err != nil {
		return
	}
	f := os.NewFile(uintptr(fd), "status")
	if f == nil {
		return
	}
	defer f.Close()
	_, _ = f.Write([]byte{code})
}

func redirectStdio() {
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return
	}
	defer devnull.Close()
	redirectStdioToDevNull(int(devnull.Fd()))
}

// daemonize re-executes this program with a status pipe, mirrors the
// fork/pipe setup of the C helper, and waits until the child has bound its
// listener and written the port file.
func daemonize(args []string) error {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		return err
	}
	defer readEnd.Close()

	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), daemonEnv+"=1", statusFDEnv+"=3")
	cmd.ExtraFiles = []*os.File{writeEnd}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		writeEnd.Close()
		return fmt.Errorf("starting daemon child: %w", err)
	}
	writeEnd.Close()

	type readResult struct {
		buf []byte
		err error
	}
	resultCh := make(chan readResult, 1)
	go func() {
		buf := make([]byte, 1)
		_, err := io.ReadFull(readEnd, buf)
		resultCh <- readResult{buf: buf, err: err}
	}()

	select {
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("timed out waiting for daemon child to start")
	case result := <-resultCh:
		if result.err == nil && len(result.buf) == 1 && result.buf[0] == '0' {
			_ = cmd.Process.Release()
			return nil
		}
		_ = cmd.Wait()
		return fmt.Errorf("daemon child failed during setup")
	}
}

// watchRoot exits when the served directory (or the target of a symlinked
// directory) disappears. The C helper uses a GFileMonitor; polling keeps this
// implementation dependency-free and is sufficient for test teardown.
func watchRoot(root string, srv *http.Server, closeOnce *sync.Once) {
	ticker := time.NewTicker(autoexitInterval)
	defer ticker.Stop()
	for range ticker.C {
		fi, err := os.Lstat(root)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				closeOnce.Do(func() { _ = srv.Close() })
				return
			}
			continue
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			if _, err := os.Stat(root); err != nil && errors.Is(err, fs.ErrNotExist) {
				closeOnce.Do(func() { _ = srv.Close() })
				return
			}
		}
	}
}

func serve(opts *options) error {
	var logWriter io.Writer
	var logCloser io.Closer
	switch opts.logFile {
	case "":
	case "-":
		logWriter = os.Stdout
	default:
		f, err := os.OpenFile(opts.logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			notifyStatus('1')
			return fmt.Errorf("opening log file %s: %w", opts.logFile, err)
		}
		logWriter = f
		logCloser = f
	}
	if logCloser != nil {
		defer logCloser.Close()
	}

	s, err := newServer(opts, logWriter)
	if err != nil {
		notifyStatus('1')
		return err
	}

	addr := opts.address
	if addr == "" {
		addr = "0.0.0.0"
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(addr, strconv.Itoa(opts.port)))
	if err != nil {
		notifyStatus('1')
		return fmt.Errorf("listening on %s: %w", addr, err)
	}

	if opts.portFile != "" {
		tcpAddr, ok := ln.Addr().(*net.TCPAddr)
		if !ok {
			ln.Close()
			notifyStatus('1')
			return fmt.Errorf("unexpected listener address %s", ln.Addr())
		}
		if err := writePortFile(opts.portFile, tcpAddr.Port); err != nil {
			ln.Close()
			notifyStatus('1')
			return fmt.Errorf("writing port file %s: %w", opts.portFile, err)
		}
	}

	if os.Getenv(daemonEnv) == "1" {
		if err := newSession(); err != nil {
			ln.Close()
			notifyStatus('1')
			return fmt.Errorf("setsid: %w", err)
		}
		notifyStatus('0')
		redirectStdio()
	} else {
		setParentDeathSignal()
	}

	s.logf("serving at root %s\n", s.root)

	errLogWriter := logWriter
	if errLogWriter == nil {
		errLogWriter = io.Discard
	}
	srv := &http.Server{
		Handler:  s,
		ErrorLog: log.New(errLogWriter, "", 0),
	}

	var closeOnce sync.Once
	if opts.autoexit {
		go watchRoot(s.root, srv, &closeOnce)
	}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		closeOnce.Do(func() { _ = srv.Close() })
	}()

	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		s.logf("root directory removed, exiting\n")
		return nil
	}
	return err
}

func run(args []string) error {
	opts, err := parseArgs(args)
	if err != nil {
		return err
	}
	if opts.showHelp {
		usage(os.Stdout)
		return nil
	}
	if opts.showVersion {
		fmt.Fprintln(os.Stdout, "testhttpd (pure Go OSTree test server)")
		return nil
	}
	if err := opts.validate(); err != nil {
		return err
	}
	if opts.daemonize && os.Getenv(daemonEnv) != "1" {
		return daemonize(args)
	}
	return serve(opts)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		notifyStatus('1')
		os.Exit(1)
	}
}
