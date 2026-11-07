package pull

import (
	"fmt"
	"net/url"
	"strings"
)

// resolveMirrorSpec expands a remote URL option into a list of mirror base
// URLs.  A "mirrorlist=" prefix downloads and validates the list; any other
// value is a single base URL.  This mirrors compute_effective_mirrorlist()
// and fetch_mirrorlist() in ostree-repo-pull.c.
func resolveMirrorSpec(f *fetcher, spec string) ([]string, error) {
	if strings.HasPrefix(spec, "mirrorlist=") {
		mirrorlistURL := strings.TrimPrefix(spec, "mirrorlist=")
		body, err := f.fetchAbsolute(mirrorlistURL)
		if err != nil {
			return nil, fmt.Errorf("While fetching mirrorlist '%s': %w", mirrorlistURL, err)
		}
		return parseMirrorList(f, mirrorlistURL, body)
	}
	if spec == "" {
		return nil, fmt.Errorf("No URL configured")
	}
	return []string{spec}, nil
}

// parseMirrorList parses the newline-separated mirrorlist format.  As in C,
// only http(s) entries are accepted and the first working mirror (one whose
// "config" file can be fetched) is kept before any later entries.
func parseMirrorList(f *fetcher, source string, body []byte) ([]string, error) {
	var out []string
	foundWorking := false
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		u, err := url.Parse(line)
		if err != nil || u.Host == "" {
			continue
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			// Local mirrors are not supported inside mirrorlists; the pull
			// code sets up local repositories differently.
			continue
		}
		base := strings.TrimSuffix(line, "/")
		if !foundWorking {
			configURL := joinURL(base, "config")
			if _, err := f.fetchAbsolute(configURL); err != nil {
				continue
			}
			foundWorking = true
		}
		out = append(out, base)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("No valid mirrors were found in mirrorlist '%s'", source)
	}
	return out, nil
}
