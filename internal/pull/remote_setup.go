package pull

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/myml/ostree-go/internal/repo"
)

// RemoteConfig is the transport-relevant subset of a [remote "..."] group.
type RemoteConfig struct {
	Name       string
	URL        string
	ContentURL string
	Metalink   string
	// HTTPHeaders are configured headers (from "http-header" /
	// "http-headers" keys); pull's --http-header option adds to these.
	HTTPHeaders []string
}

// LoadRemoteConfig reads a remote's transport options.
func LoadRemoteConfig(r *repo.Repo, name string) (*RemoteConfig, error) {
	section := repo.RemoteSection(name)
	if !r.Config().HasGroup(section) {
		return nil, fmt.Errorf("Remote \"%s\" not found", name)
	}
	cfg := &RemoteConfig{Name: name}
	if v, err := r.Config().Get(section, "url"); err == nil {
		cfg.URL = v
	}
	if v, err := r.Config().Get(section, "contenturl"); err == nil {
		cfg.ContentURL = v
	}
	if v, err := r.Config().Get(section, "metalink"); err == nil {
		cfg.Metalink = v
	}
	if v, err := r.Config().Get(section, "http-header"); err == nil && v != "" {
		cfg.HTTPHeaders = append(cfg.HTTPHeaders, v)
	}
	if list, err := r.Config().GetList(section, "http-headers"); err == nil {
		cfg.HTTPHeaders = append(cfg.HTTPHeaders, list...)
	}
	return cfg, nil
}

// prepareRemote resolves the effective metadata/content mirror lists (and,
// for metalink remotes, downloads the summary) before the pull proper starts.
//
// The returned summary bytes are non-nil only for metalink pulls.
func prepareRemote(opts *RemoteOptions) (*fetcher, []byte, error) {
	cfg := opts.RemoteConfig
	if cfg == nil && opts.Repo != nil && opts.Remote != "" {
		loaded, err := LoadRemoteConfig(opts.Repo, opts.Remote)
		if err != nil {
			return nil, nil, err
		}
		cfg = loaded
	}
	if cfg == nil {
		cfg = &RemoteConfig{Name: opts.Remote}
	}

	f := newBaseFetcher()
	if opts.NetworkRetriesSet {
		f.retries = opts.NetworkRetries
	}
	if f.retries < 0 {
		f.retries = defaultNetworkRetries
	}
	if opts.AppendUserAgent != "" {
		f.userAgent = f.userAgent + " " + opts.AppendUserAgent
	}
	headers := append([]string(nil), cfg.HTTPHeaders...)
	headers = append(headers, opts.HTTPHeaders...)
	for _, kv := range headers {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			return nil, nil, fmt.Errorf("Missing '=' in --http-header")
		}
		f.headers = append(f.headers, httpHeader{Name: kv[:i], Value: kv[i+1:]})
	}

	f.dest = opts.Repo
	f.dryRun = opts.DryRun
	f.cacheDir = opts.CacheDir
	if opts.Repo != nil && opts.Remote != "" {
		jar := filepath.Join(opts.Repo.Path(), opts.Remote+".cookies.txt")
		if st, err := os.Stat(jar); err == nil && st.Mode().IsRegular() {
			cookies, err := loadCookieFile(jar)
			if err != nil {
				return nil, nil, err
			}
			f.cookies = cookies
		}
	}
	for _, cachePath := range opts.LocalCacheRepos {
		cacheRepo, err := repo.Open(cachePath)
		if err != nil {
			return nil, nil, fmt.Errorf("opening local cache repo %s: %w", cachePath, err)
		}
		f.caches = append(f.caches, cacheRepo)
	}

	// The metadata base URL: --url overrides the configured url.  A configured
	// metalink takes precedence over both, matching ostree-repo-pull.c.
	urlOverride := opts.URLOverride
	if urlOverride == "" {
		if cfg.URL != "" {
			urlOverride = cfg.URL
		} else {
			urlOverride = opts.URL
		}
	}

	if cfg.Metalink != "" {
		base, summary, err := resolveMetalink(f, cfg.Metalink)
		if err != nil {
			return nil, nil, err
		}
		if err := f.setMetaMirrors([]string{base}); err != nil {
			return nil, nil, err
		}
		contentSpec := cfg.ContentURL
		if contentSpec == "" {
			f.contentMirrors = append([]string(nil), f.metaMirrors...)
		} else {
			mirrors, err := resolveMirrorSpec(f, contentSpec)
			if err != nil {
				return nil, nil, err
			}
			if err := f.setContentMirrors(mirrors); err != nil {
				return nil, nil, err
			}
		}
		return f, summary, nil
	}

	if urlOverride == "" {
		return nil, nil, fmt.Errorf("No \"url\" option in remote")
	}
	metaMirrors, err := resolveMirrorSpec(f, urlOverride)
	if err != nil {
		return nil, nil, err
	}
	if err := f.setMetaMirrors(metaMirrors); err != nil {
		return nil, nil, err
	}

	// Content objects come from contenturl when set.  With --url, libostree
	// uses the override for content too (unless a metalink is configured).
	contentSpec := ""
	if opts.URLOverride != "" {
		contentSpec = opts.URLOverride
	} else {
		contentSpec = cfg.ContentURL
	}
	if contentSpec == "" {
		f.contentMirrors = append([]string(nil), f.metaMirrors...)
	} else {
		contentMirrors, err := resolveMirrorSpec(f, contentSpec)
		if err != nil {
			return nil, nil, err
		}
		if err := f.setContentMirrors(contentMirrors); err != nil {
			return nil, nil, err
		}
	}
	return f, nil, nil
}

// FetchRemoteSummary fetches a remote's summary using its configured
// transport (url, mirrorlist or metalink).  cacheDir is the optional
// --cache-dir override applied by the `remote refs` and `remote summary`
// commands.
func FetchRemoteSummary(r *repo.Repo, name, cacheDir string) ([]byte, error) {
	opts := RemoteOptions{
		Repo:     r,
		Remote:   name,
		DryRun:   true,
		CacheDir: cacheDir,
	}
	f, summary, err := prepareRemote(&opts)
	if err != nil {
		return nil, err
	}
	if summary != nil {
		return summary, nil
	}
	return f.get("summary")
}
