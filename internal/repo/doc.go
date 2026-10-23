// Package repo implements the OSTree repository storage layer (L4).
//
// It covers the on-disk repository layout, the five repository modes,
// GKeyFile-compatible configuration, refs, repository locking and loose object
// storage. Object payloads are deliberately treated as opaque bytes: GVariant
// encoding and decoding of commits, dirtrees and file headers belong to
// internal/object. The only transformation performed here is the raw-deflate
// wrapper used for file objects in archive repositories.
//
// The implementation follows libostree 2025.2 behaviour where it is visible on
// disk (paths, modes, ref semantics, locking strategy). See README-style notes
// in GO-REWRITE.md sections 4.3 and 4.4 for the format contract.
package repo
