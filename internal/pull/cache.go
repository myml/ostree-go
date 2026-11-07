package pull

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/myml/ostree-go/internal/repo"
)

// lookupLocalCache checks the --localcache-repo repositories for an object.
// A hit is imported into the destination repository and reported so the pull
// status can account for it separately from network fetches.
func (f *fetcher) lookupLocalCache(checksum string, typ repo.ObjectType) ([]byte, bool, error) {
	if f.dest == nil || len(f.caches) == 0 {
		return nil, false, nil
	}
	for _, cache := range f.caches {
		has, err := cache.HasObject(checksum, typ)
		if err != nil || !has {
			continue
		}
		if !cacheCompatible(cache, f.dest, typ) {
			continue
		}
		data, err := cache.ReadObject(checksum, typ)
		if err != nil {
			continue
		}
		if !f.dryRun {
			if err := importObject(f.dest, checksum, typ, data); err != nil {
				return nil, false, err
			}
		}
		return data, true, nil
	}
	return nil, false, nil
}

// cacheCompatible reports whether an object's on-disk representation is
// identical in both repositories.  Metadata always is; content differs
// between archive and bare modes and is therefore only reused for equal
// modes (the test-pull-localcache scenario uses two archive repos).
func cacheCompatible(src, dst *repo.Repo, typ repo.ObjectType) bool {
	if typ.IsMeta() {
		return true
	}
	return src.Mode() == dst.Mode()
}

// importObject copies an object from a cache repository into dst.
func importObject(dst *repo.Repo, checksum string, typ repo.ObjectType, data []byte) error {
	if typ.IsMeta() || typ == repo.ObjectPayloadLink {
		got, err := dst.WriteObject(typ, data)
		if err != nil {
			return err
		}
		if got != checksum {
			return fmt.Errorf("checksum mismatch importing %s.%s", checksum, typ)
		}
		return nil
	}
	return dst.WriteRawObject(checksum, typ, data)
}

// cachePath returns the --cache-dir path used for an object payload.
func (f *fetcher) cachePath(checksum string, typ repo.ObjectType) string {
	if f.cacheDir == "" {
		return ""
	}
	return filepath.Join(f.cacheDir, "objects", checksum[:2],
		checksum[2:]+"."+typ.String())
}

func (f *fetcher) readDiskCache(checksum string, typ repo.ObjectType) ([]byte, bool) {
	p := f.cachePath(checksum, typ)
	if p == "" {
		return nil, false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	return data, true
}

func (f *fetcher) writeDiskCache(checksum string, typ repo.ObjectType, data []byte) {
	p := f.cachePath(checksum, typ)
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(p, data, 0o644)
}
