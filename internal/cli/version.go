package cli

import "fmt"

// writeVersion emits the docker-style YAML used by `ostree --version`.
// The format matches the C CLI byte for byte: a Git line is only printed
// when the revision is known, and DevelBuild only for devel builds.
func (a *App) writeVersion() {
	fmt.Fprintf(a.Stdout, "%s:\n", a.PackageName)
	fmt.Fprintf(a.Stdout, " Version: '%s'\n", a.Version)
	if a.Git != "" {
		fmt.Fprintf(a.Stdout, " Git: %s\n", a.Git)
	}
	if a.DevelBuild {
		fmt.Fprintf(a.Stdout, " DevelBuild: yes\n")
	}
	fmt.Fprintf(a.Stdout, " Features:\n")
	for _, feature := range a.Features {
		fmt.Fprintf(a.Stdout, "  - %s\n", feature)
	}
}
