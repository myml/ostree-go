package pull

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// cookie is one entry of a libsoup-style text cookie jar.  The on-disk format
// is the Netscape cookie file format used by
// ot-remote-cookie-util.c:
//
//	domain \t flag \t path \t secure \t expiration \t name \t value
type cookie struct {
	Domain     string
	Flag       string
	Path       string
	Secure     string
	Expiration int64
	Name       string
	Value      string
}

func parseCookieLine(line string) (cookie, bool) {
	fields := strings.Split(line, "\t")
	if len(fields) < 7 {
		return cookie{}, false
	}
	// Values may contain tabs in theory; libostree's sscanf splits on the
	// first six separators and takes the remainder as the value.
	fields = append(fields[:6], strings.Join(fields[6:], "\t"))
	expiration, err := strconv.ParseInt(strings.TrimSpace(fields[4]), 10, 64)
	if err != nil {
		return cookie{}, false
	}
	return cookie{
		Domain:     fields[0],
		Flag:       fields[1],
		Path:       fields[2],
		Secure:     fields[3],
		Expiration: expiration,
		Name:       fields[5],
		Value:      fields[6],
	}, true
}

// loadCookieFile reads a cookie jar.  A missing file yields no cookies.
func loadCookieFile(path string) ([]cookie, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []cookie
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		if c, ok := parseCookieLine(line); ok {
			out = append(out, c)
		}
	}
	return out, nil
}

// cookieHeader builds the Cookie request header for u.
func cookieHeader(cookies []cookie, u *url.URL) string {
	if u == nil {
		return ""
	}
	now := time.Now().Unix()
	host := u.Hostname()
	requestPath := u.EscapedPath()
	if requestPath == "" {
		requestPath = "/"
	}
	var parts []string
	for _, c := range cookies {
		if c.Expiration != 0 && c.Expiration <= now {
			continue
		}
		if strings.EqualFold(c.Secure, "TRUE") && u.Scheme != "https" {
			continue
		}
		if !cookieDomainMatches(c, host) {
			continue
		}
		if c.Path != "" && !strings.HasPrefix(requestPath, c.Path) {
			continue
		}
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

func cookieDomainMatches(c cookie, host string) bool {
	domain := c.Domain
	if strings.EqualFold(c.Flag, "TRUE") || strings.HasPrefix(domain, ".") {
		domain = strings.TrimPrefix(domain, ".")
		if host == domain {
			return true
		}
		return strings.HasSuffix(host, "."+domain)
	}
	return strings.EqualFold(host, strings.TrimPrefix(domain, "."))
}

// AddCookieFile appends a cookie to the remote's jar.
func AddCookieFile(path, domain, cookiePath, name, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("open(%s): %w", path, err)
	}
	defer f.Close()
	expires := time.Now().UTC().AddDate(25, 0, 0).Unix()
	flag := "FALSE"
	if strings.HasPrefix(domain, ".") {
		flag = "TRUE"
	}
	line := fmt.Sprintf("%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
		domain, flag, cookiePath, "FALSE", expires, name, value)
	if _, err := io.WriteString(f, line); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// DeleteCookieFile removes matching cookies, rewriting the jar in place.
func DeleteCookieFile(path, domain, cookiePath, name string) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	found := false
	var b strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		c, ok := parseCookieLine(line)
		if !ok {
			continue
		}
		if c.Domain == domain && c.Path == cookiePath && c.Name == name {
			found = true
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return err
	}
	if !found {
		return errors.New("Cookie not found in jar")
	}
	return nil
}

// ListCookieFile prints the jar in the format of `ostree remote list-cookies`.
func ListCookieFile(path string, w io.Writer) error {
	cookies, err := loadCookieFile(path)
	if err != nil {
		return err
	}
	for _, c := range cookies {
		fmt.Fprintln(w, "--")
		fmt.Fprintf(w, "Domain: %s\n", c.Domain)
		fmt.Fprintf(w, "Path: %s\n", c.Path)
		fmt.Fprintf(w, "Name: %s\n", c.Name)
		fmt.Fprintf(w, "Secure: %s\n", c.Secure)
		if c.Expiration != 0 {
			fmt.Fprintf(w, "Expires: %s\n",
				time.Unix(c.Expiration, 0).UTC().Format("2006-01-02 15:04:05 +0000"))
		}
		fmt.Fprintf(w, "Value: %s\n", c.Value)
	}
	return nil
}
