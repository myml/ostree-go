# L1 CLI golden files

These files were captured from the reference C implementation:

    /usr/bin/ostree --version  ->  libostree 2025.2 (see c-reference-version.txt)
    LC_ALL=C
    argv[0] = /usr/bin/ostree
    cwd = empty temporary directory
    OSTREE_REPO = <temporary directory>/nonexistent-repo

Each case in `cases.tsv` has three files:

* `<case>.out`  – reference stdout
* `<case>.err`  – reference stderr
* `<case>.exit` – reference exit status (0 or 1)

Regenerate them with:

    OSTREE_BIN=/usr/bin/ostree go/hack/gen-cli-golden.sh

The Go test (`internal/cli`) reads these files and never executes the C
binary, so it also runs in environments without an ostree installation.
Before diffing, the test normalizes:

* the program path used in usage lines (`/usr/bin/ostree`,
  `/opt/go-ostree/ostree` -> `@PROG@`), because the Go binary may be
  invoked under any name;
* the optional ` Git:` line and the contents of the `Features:` list in
  `--version` output, because they depend on the build.  The YAML shape
  itself is still compared, and separate unit tests pin the exact
  rendering.
