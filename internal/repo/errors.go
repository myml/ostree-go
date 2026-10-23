package repo

import "errors"

// Sentinel errors returned (usually wrapped) by this package. Callers should
// use errors.Is to classify failures.
var (
	// ErrNotARepo means the directory does not contain an objects/ directory.
	ErrNotARepo = errors.New("not an ostree repository")
	// ErrReadOnly means a write was attempted on a non-writable repository.
	ErrReadOnly = errors.New("repository is not writable")
	// ErrInvalidMode means a repository mode string or value is invalid.
	ErrInvalidMode = errors.New("invalid repository mode")
	// ErrInvalidObjectType means an object type value or suffix is invalid.
	ErrInvalidObjectType = errors.New("invalid object type")
	// ErrInvalidChecksum means a string is not a lowercase 64 character sha256.
	ErrInvalidChecksum = errors.New("invalid checksum")
	// ErrInvalidRef means a ref name is syntactically invalid.
	ErrInvalidRef = errors.New("invalid ref name")
	// ErrInvalidRemote means a remote name is syntactically invalid.
	ErrInvalidRemote = errors.New("invalid remote name")
	// ErrInvalidCollection means a collection ID is syntactically invalid.
	ErrInvalidCollection = errors.New("invalid collection ID")
	// ErrRefConflict means a ref cannot be written because a file/directory
	// conflict exists (for example writing "foo" while "foo/bar" exists).
	ErrRefConflict = errors.New("ref conflict")
	// ErrRefIsDirectory means a ref path exists but is a directory. libostree
	// reports G_IO_ERROR_IS_DIRECTORY in this case and callers such as
	// `ostree refs --create` treat it as "no ref here".
	ErrRefIsDirectory = errors.New("ref is a directory")
	// ErrObjectNotFound is returned by ReadObject when the loose object does
	// not exist. It wraps fs.ErrNotExist as well.
	ErrObjectNotFound = errors.New("object not found")
	// ErrLockWouldBlock means an incompatible lock is currently held.
	ErrLockWouldBlock = errors.New("repository lock is held")
	// ErrLockTimeout means the configured lock timeout expired. It also wraps
	// ErrLockWouldBlock.
	ErrLockTimeout = errors.New("timed out waiting for repository lock")
	// ErrGroupNotFound means a config group does not exist.
	ErrGroupNotFound = errors.New("config group not found")
	// ErrKeyNotFound means a config key does not exist.
	ErrKeyNotFound = errors.New("config key not found")
	// ErrInvalidConfig means the config file could not be parsed or a value
	// could not be interpreted.
	ErrInvalidConfig = errors.New("invalid config")
	// ErrUnsupported means the operation is not implemented on this platform
	// or by this milestone.
	ErrUnsupported = errors.New("unsupported operation")
)
