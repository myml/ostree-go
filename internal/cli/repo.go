package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Repo is the minimal repository handle the CLI layer needs.  The real
// repository implementation (internal/repo) is injected through
// App.OpenRepo once it exists; L1 only needs to know the path it opened
// so that commands can report it.
type Repo interface {
	// Path is the repository root path.
	Path() string
}

// RepoOpener opens the repository at path.  It must return an error
// whose message is suitable for direct "error: ..." output (the default
// implementation mimics libostree's "opening repo: ..." prefix).
type RepoOpener func(path string) (Repo, error)

// RepoOpenError is returned by the default opener.  NotFound mirrors the
// G_IO_ERROR_NOT_FOUND case that makes the CLI print the command help
// plus "Command requires a --repo argument" when the *default*
// repository cannot be opened.
type RepoOpenError struct {
	Message  string
	NotFound bool
}

func (e *RepoOpenError) Error() string { return e.Message }

// IsRepoNotFound reports whether err represents G_IO_ERROR_NOT_FOUND.
func IsRepoNotFound(err error) bool {
	var repoErr *RepoOpenError
	return errors.As(err, &repoErr) && repoErr.NotFound
}

type defaultRepo struct {
	path string
}

func (r *defaultRepo) Path() string { return r.path }

func newRepoOpenError(format string, args ...any) *RepoOpenError {
	return &RepoOpenError{Message: fmt.Sprintf(format, args...)}
}

func newRepoNotFoundError(format string, args ...any) *RepoOpenError {
	return &RepoOpenError{Message: fmt.Sprintf(format, args...), NotFound: true}
}

// OpenRepoDefault is the L1 placeholder implementation of
// ostree_repo_open().  It validates the repository layout that libostree
// checks first (the directory itself and its objects/ subdirectory) and
// formats errno-style messages the same way glnx_opendirat() does.
//
// TODO(L2/L3): replace this with internal/repo once available.  It
// deliberately does not parse config, objects/00 etc.; see the L1 known
// differences list.
func OpenRepoDefault(path string) (Repo, error) {
	// g_file_new_for_path("") produces a NULL path in the C code and
	// openat() then reports EFAULT.  Preserve the (odd) message.
	if path == "" {
		return nil, newRepoOpenError("opening repo: opendir((null)): Bad address")
	}

	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, newRepoNotFoundError("opening repo: opendir(%s): %s", path, errnoText(err))
		}
		return nil, newRepoOpenError("opening repo: opendir(%s): %s", path, errnoText(err))
	}
	if !info.IsDir() {
		return nil, newRepoOpenError("opening repo: opendir(%s): %s", path, "Not a directory")
	}
	if _, err := os.Stat(filepath.Join(path, "objects")); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, newRepoNotFoundError("opening repo: opendir(objects): %s", errnoText(err))
		}
		return nil, newRepoOpenError("opening repo: opendir(objects): %s", errnoText(err))
	}

	return &defaultRepo{path: path}, nil
}

// errnoText renders an error the way strerror(3) would, with the leading
// capital letter that glib error messages use.
func errnoText(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		text := errno.Error()
		if text != "" {
			return strings.ToUpper(text[:1]) + text[1:]
		}
	}
	if errors.Is(err, fs.ErrNotExist) {
		return "No such file or directory"
	}
	if errors.Is(err, fs.ErrPermission) {
		return "Permission denied"
	}
	return err.Error()
}
