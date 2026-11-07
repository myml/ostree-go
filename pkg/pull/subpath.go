package pull

import "strings"

// pullSubpathMatches reports whether the entry dirPath+basename of a dirtree
// should be fetched when only the given subpaths were requested. It is a
// direct translation of matches_pull_dir() in ostree-repo-pull.c: a subpath
// matches itself, the directories leading to it, and everything below it.
//
// dirPath always starts and ends with '/' ("/" for the root) and basename is
// a single path component.
func pullSubpathMatches(subpaths []string, dirPath, basename string, basenameIsDir bool) bool {
	if len(subpaths) == 0 {
		return true
	}
	currentFile := dirPath + basename
	for _, pullDir := range subpaths {
		if pullDirMatches(currentFile, pullDir, basenameIsDir) {
			return true
		}
	}
	return false
}

// pullDirMatches implements matches_pull_dir().
func pullDirMatches(currentFile, pullDir string, currentFileIsDir bool) bool {
	if strings.HasPrefix(pullDir, currentFile) {
		rest := pullDir[len(currentFile):]
		if rest == "" {
			// The current file is exactly the requested path; match it even
			// when it is not a directory (e.g. --subpath=/firstfile).
			return true
		}
		if rest[0] == '/' {
			// currentFile is a directory prefix of pullDir.
			return currentFileIsDir
		}
		// Prefix matched a partial component (e.g. /foo of /fooo).
		return false
	}
	if strings.HasPrefix(currentFile, pullDir) {
		rest := currentFile[len(pullDir):]
		// Only match when the prefix ended on a component boundary.
		return len(rest) > 0 && rest[0] == '/'
	}
	return false
}
