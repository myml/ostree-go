#!/usr/bin/env bash
# Run OSTree's upstream shell tests against a Go-built `ostree` binary.
#
# The tests are executed directly (`bash tests/test-X.sh`) from a fresh empty
# temporary directory each. A fake build directory is assembled with:
#   - the Go binary as `ostree` (and the conventional `tests/ostree` symlink)
#   - the pure Go `ostree-trivial-httpd` replacement
#   - small shims for `setfattr`, `getfattr` and `get-byte-order` when the host
#     does not provide the corresponding helpers
# G_TEST_SRCDIR / G_TEST_BUILDDIR point at the source tree and the fake
# build directory respectively. `OSTREE_UNINSTALLED` is deliberately not set.
#
# `make check` is never used.

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
GO_ROOT=$(cd "${SCRIPT_DIR}/.." && pwd)
SRC_ROOT=$(cd "${GO_ROOT}/.." && pwd)

GO_CMD=${GO:-/usr/local/go/bin/go}
if ! command -v "${GO_CMD}" >/dev/null 2>&1; then
    GO_CMD=$(command -v go || true)
fi
if [ -z "${GO_CMD}" ]; then
    echo "error: no Go toolchain found (set GO=/path/to/go)" >&2
    exit 2
fi

# Conservative read-only subset: no network, no sysroot/deploy, no root, no
# GPG/ed25519 and no optional C helpers. The canonical test list lives in
# Makefile-tests.am; see --list --all for every discovered script.
DEFAULT_TESTS=(
    test-config
    test-refs
    test-refs-collections
    test-init-collections
    test-summary-view
    test-summary-update
    test-auto-summary
    test-parent
    test-archivez
)

# Scripts that are intentionally not part of --all, with the reason.
EXCLUDED_TESTS=(
    "test-composefs:composefs is out of scope for this phase"
    "test-admin-deploy-composefs:composefs is out of scope for this phase"
    "test-symbols:requires the C shared library build"
    "test-delta-fallocate:upstream dist_uninstalled test, needs C helper/OSTREE_UNINSTALLED"
    "test-rofiles-fuse:requires the C rofiles-fuse helper"
    "test-libarchive:requires the C test-libarchive-import helper"
    "test-create-usb:requires the C repo-finder-mount helper"
    "test-repo-finder-mount-integration:requires the C repo-finder-mount helper"
    "test-corruption:requires gjs"
    "test-pull-corruption:requires gjs"
)

usage() {
    cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Run upstream OSTree shell tests (TAP) against the Go implementation.

Options:
  --list                 List the default read-only subset and exit
  --list --all           List every discovered test with its category
  --test NAME            Run one test (NAME may omit the test- prefix/.sh)
  --all                  Run every test except the documented exclusions
  --default              Run the conservative read-only subset (default)
  --ostree-binary PATH   Use PATH instead of building ./cmd/ostree
  --httpd-binary PATH    Use PATH instead of building ./internal/testhttpd
  --builddir DIR         Reuse DIR as the fake build directory
  --no-build             Do not build; require an existing binary in builddir
  --timeout SECONDS      Per-test timeout (default: 300)
  --keep                 Keep logs and per-test temporary directories
  --verbose, -v          Echo test output as it runs
  -h, --help             Show this help

Environment:
  GO                     Go toolchain to use (default: /usr/local/go/bin/go)
  TMPDIR                 Root for the fake build directory and test tmpdirs

Exit status: 0 all selected tests passed/skipped, 1 test failure, 2 setup error.
EOF
}

list_default=false
list_all=false
run_all=false
run_default=true
tests=()
ostree_binary=""
httpd_binary=""
builddir=""
no_build=false
timeout_secs=300
keep=false
verbose=false

while [ $# -gt 0 ]; do
    case "$1" in
        --list)
            list_default=true
            shift
            ;;
        --test)
            [ $# -ge 2 ] || { echo "error: --test requires a name" >&2; exit 2; }
            tests+=("$2")
            run_default=false
            shift 2
            ;;
        --test=*)
            tests+=("${1#--test=}")
            run_default=false
            shift
            ;;
        --all)
            run_all=true
            run_default=false
            shift
            ;;
        --default)
            run_default=true
            run_all=false
            shift
            ;;
        --ostree-binary)
            [ $# -ge 2 ] || { echo "error: --ostree-binary requires a path" >&2; exit 2; }
            ostree_binary=$2
            shift 2
            ;;
        --httpd-binary)
            [ $# -ge 2 ] || { echo "error: --httpd-binary requires a path" >&2; exit 2; }
            httpd_binary=$2
            shift 2
            ;;
        --builddir)
            [ $# -ge 2 ] || { echo "error: --builddir requires a path" >&2; exit 2; }
            builddir=$2
            shift 2
            ;;
        --no-build)
            no_build=true
            shift
            ;;
        --timeout)
            [ $# -ge 2 ] || { echo "error: --timeout requires seconds" >&2; exit 2; }
            timeout_secs=$2
            shift 2
            ;;
        --keep)
            keep=true
            shift
            ;;
        -v|--verbose)
            verbose=true
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            echo "error: unknown option: $1" >&2
            usage >&2
            exit 2
            ;;
    esac
done

if [ "${list_default}" = true ] && [ "${run_all}" = true ]; then
    list_all=true
fi

normalize_test_name() {
    local name=$1
    name=${name%.sh}
    case "${name}" in
        test-*) printf '%s\n' "${name}" ;;
        *) printf 'test-%s\n' "${name}" ;;
    esac
}

discover_all_tests() {
    local script
    for script in "${SRC_ROOT}"/tests/test-*.sh; do
        [ -e "${script}" ] || continue
        basename "${script}" .sh
    done | LC_ALL=C sort
}

exclusion_reason() {
    local wanted=$1 entry
    for entry in "${EXCLUDED_TESTS[@]}"; do
        if [ "${entry%%:*}" = "${wanted}" ]; then
            printf '%s\n' "${entry#*:}"
            return 0
        fi
    done
    return 1
}

is_default_test() {
    local wanted=$1 candidate
    for candidate in "${DEFAULT_TESTS[@]}"; do
        [ "${candidate}" = "${wanted}" ] && return 0
    done
    return 1
}

print_list() {
    local -a all=()
    mapfile -t all < <(discover_all_tests)
    local name reason
    if [ "${list_all}" = true ]; then
        echo "Discovered test scripts (${#all[@]}) from ${SRC_ROOT}/tests:"
        for name in "${all[@]}"; do
            if reason=$(exclusion_reason "${name}"); then
                printf '  %-42s excluded (%s)\n' "${name}" "${reason}"
            elif is_default_test "${name}"; then
                printf '  %-42s readonly (default)\n' "${name}"
            else
                printf '  %-42s full\n' "${name}"
            fi
        done
        echo
        echo "Default read-only subset (${#DEFAULT_TESTS[@]}):"
    else
        echo "Default read-only subset (${#DEFAULT_TESTS[@]}):"
    fi
    for name in "${DEFAULT_TESTS[@]}"; do
        if [ -f "${SRC_ROOT}/tests/${name}.sh" ]; then
            printf '  %s\n' "${name}"
        else
            printf '  %s (MISSING: not present in this tree)\n' "${name}"
        fi
    done
}

if [ "${list_default}" = true ]; then
    print_list
    exit 0
fi

selected=()
if [ ${#tests[@]} -gt 0 ]; then
    for name in "${tests[@]}"; do
        name=$(normalize_test_name "${name}")
        if [ ! -f "${SRC_ROOT}/tests/${name}.sh" ]; then
            echo "error: no such test: ${name} (${SRC_ROOT}/tests/${name}.sh)" >&2
            echo "hint: use --list --all to see available tests" >&2
            exit 2
        fi
        if reason=$(exclusion_reason "${name}"); then
            echo "warning: ${name} is normally excluded: ${reason}" >&2
        fi
        selected+=("${name}")
    done
elif [ "${run_all}" = true ]; then
    while IFS= read -r name; do
        if ! exclusion_reason "${name}" >/dev/null; then
            selected+=("${name}")
        fi
    done < <(discover_all_tests)
else
    # run_default
    for name in "${DEFAULT_TESTS[@]}"; do
        if [ -f "${SRC_ROOT}/tests/${name}.sh" ]; then
            selected+=("${name}")
        else
            echo "warning: default test ${name} is missing from this tree; skipping" >&2
        fi
    done
fi

if [ ${#selected[@]} -eq 0 ]; then
    echo "error: no tests selected" >&2
    exit 2
fi

tmp_root=${TMPDIR:-/tmp}
if [ -z "${builddir}" ]; then
    workdir=$(mktemp -d "${tmp_root%/}/ostree-conformance.XXXXXX")
    builddir="${workdir}/builddir"
    own_workdir=true
else
    workdir=${builddir}
    own_workdir=false
fi
mkdir -p "${builddir}/tests"
builddir=$(cd "${builddir}" && pwd)
logs_dir="${builddir}/logs"
mkdir -p "${logs_dir}"

cleanup() {
    if [ "${keep}" = false ] && [ "${own_workdir}" = true ]; then
        rm -rf "${workdir}"
    fi
}
trap cleanup EXIT

build_ostree() {
    if [ -n "${ostree_binary}" ]; then
        if [ ! -x "${ostree_binary}" ]; then
            echo "error: --ostree-binary ${ostree_binary} is not executable" >&2
            return 1
        fi
        ln -sfn "$(cd "$(dirname "${ostree_binary}")" && pwd)/$(basename "${ostree_binary}")" "${builddir}/ostree"
        echo "Using existing ostree binary: ${ostree_binary}"
        return 0
    fi
    if [ "${no_build}" = true ]; then
        if [ -x "${builddir}/ostree" ]; then
            echo "Using existing ostree binary: ${builddir}/ostree"
            return 0
        fi
        echo "error: --no-build but ${builddir}/ostree does not exist" >&2
        return 1
    fi
    if [ ! -d "${GO_ROOT}/cmd/ostree" ]; then
        echo "error: ${GO_ROOT}/cmd/ostree does not exist (Go CLI not implemented yet)" >&2
        echo "hint: use --ostree-binary /usr/bin/ostree to validate the harness" >&2
        return 1
    fi
    echo "Building Go ostree -> ${builddir}/ostree"
    if ! (cd "${GO_ROOT}" && CGO_ENABLED=0 "${GO_CMD}" build -trimpath -o "${builddir}/ostree" ./cmd/ostree); then
        echo "error: failed to build ./cmd/ostree" >&2
        return 1
    fi
}

build_httpd() {
    if [ -n "${httpd_binary}" ]; then
        if [ ! -x "${httpd_binary}" ]; then
            echo "error: --httpd-binary ${httpd_binary} is not executable" >&2
            return 1
        fi
        ln -sfn "$(cd "$(dirname "${httpd_binary}")" && pwd)/$(basename "${httpd_binary}")" "${builddir}/ostree-trivial-httpd"
        echo "Using existing test httpd: ${httpd_binary}"
        return 0
    fi
    if [ "${no_build}" = true ]; then
        if [ -x "${builddir}/ostree-trivial-httpd" ]; then
            echo "Using existing test httpd: ${builddir}/ostree-trivial-httpd"
            return 0
        fi
        echo "error: --no-build but ${builddir}/ostree-trivial-httpd does not exist" >&2
        return 1
    fi
    echo "Building Go test httpd -> ${builddir}/ostree-trivial-httpd"
    if ! (cd "${GO_ROOT}" && CGO_ENABLED=0 "${GO_CMD}" build -trimpath -o "${builddir}/ostree-trivial-httpd" ./internal/testhttpd); then
        echo "error: failed to build ./internal/testhttpd" >&2
        return 1
    fi
}

build_ostree || exit 2
build_httpd || exit 2

# The upstream build puts the binary in tests/ as well; some tests invoke
# `${test_builddir}/ostree` directly.
ln -sfn ../ostree "${builddir}/tests/ostree"
if [ ! -L "${builddir}/ostree" ]; then
    chmod +x "${builddir}/ostree"
fi

# Install helper shims only when the host lacks the real tools. `setfattr` is
# mandatory for libtest.sh's xattr probing, even in read-only tests.
shim_dir="${SCRIPT_DIR}/shims"
for shim in setfattr getfattr get-byte-order; do
    if [ -x "${shim_dir}/${shim}" ]; then
        if ! command -v "${shim}" >/dev/null 2>&1; then
            ln -sfn "${shim_dir}/${shim}" "${builddir}/tests/${shim}"
            echo "Installed ${shim} shim (host tool not found)"
        fi
    fi
done

export PATH="${builddir}:${builddir}/tests:${PATH}"
export G_TEST_SRCDIR="${SRC_ROOT}"
export G_TEST_BUILDDIR="${builddir}"
export OSTREE_HTTPD="${builddir}/ostree-trivial-httpd"
export LC_ALL=C
export LANG=C
export TZ=UTC
export PYTHONUNBUFFERED=1
export GSETTINGS_BACKEND=memory
export GIO_USE_PROXY_RESOLVER=dummy
export GIO_USE_VFS=local
export G_DEBUG=fatal-warnings
# Deliberately do not set OSTREE_UNINSTALLED: tests needing C helpers then
# skip naturally instead of running against an incomplete fake build tree.
unset OSTREE_UNINSTALLED OSTREE_UNINSTALLED_SRCDIR || true

if ! "${builddir}/ostree" --version >/dev/null 2>&1; then
    echo "warning: '${builddir}/ostree --version' failed; most tests will fail" >&2
fi

total_pass=0
total_fail=0
total_skip=0
failed_tests=()
summary_file="${logs_dir}/summary.txt"
: >"${summary_file}"

run_one() {
    local name=$1
    local script="${SRC_ROOT}/tests/${name}.sh"
    local log="${logs_dir}/${name}.log"
    local rundir
    rundir=$(mktemp -d "${tmp_root%/}/ostree-test-${name}.XXXXXX")

    local rc=0
    if [ "${verbose}" = true ]; then
        echo "--- ${name} (cwd=${rundir}) ---"
        set +e
        ( cd "${rundir}" && timeout "${timeout_secs}" bash "${script}" ) 2>&1 | tee "${log}"
        rc=${PIPESTATUS[0]}
        set -e
    else
        set +e
        ( cd "${rundir}" && timeout "${timeout_secs}" bash "${script}" ) >"${log}" 2>&1
        rc=$?
        set -e
    fi

    local pass=0 fail=0 skip=0 plan_skip=false bail=false
    local -a failed_lines=()
    local line
    while IFS= read -r line; do
        # TAP allows "ok" without a number followed by a word boundary; the
        # upstream tap-driver uses the same rule, which matters for scripts
        # such as test-pull-resume.sh that print "ok, pull succeeded!".
        if [[ "${line}" == "ok" || "${line}" == ok[[:space:]]* || "${line}" == ok,* ]]; then
            if [[ "${line}" =~ \#[[:space:]]*[Ss][Kk][Ii][Pp] ]]; then
                skip=$((skip + 1))
            else
                pass=$((pass + 1))
            fi
        elif [[ "${line}" == "not ok" || "${line}" == "not ok"[[:space:]]* || "${line}" == "not ok,"* ]]; then
            fail=$((fail + 1))
            failed_lines+=("${line}")
        elif [[ "${line}" == "1..0"* ]]; then
            if [[ "${line}" =~ [Ss][Kk][Ii][Pp] ]]; then
                plan_skip=true
            fi
        elif [[ "${line}" == "Bail out!"* ]]; then
            bail=true
        fi
    done <"${log}"

    if [ "${plan_skip}" = true ] && [ "${pass}" -eq 0 ] && [ "${fail}" -eq 0 ]; then
        skip=1
    fi

    if [ "${rc}" -ne 0 ]; then
        if [ "${fail}" -eq 0 ]; then
            fail=1
        fi
        if [ "${rc}" -eq 124 ]; then
            failed_lines+=("test timed out after ${timeout_secs}s")
        elif [ "${rc}" -eq 134 ] && [ "${bail}" = true ]; then
            failed_lines+=("Bail out!")
        else
            failed_lines+=("script exited with status ${rc}")
        fi
    fi

    if [ "${fail}" -eq 0 ] && [ "${pass}" -eq 0 ] && [ "${skip}" -eq 0 ] && [ "${plan_skip}" = false ]; then
        # A test that produced no TAP at all and exited 0 is treated as a
        # failure: it means the script did not run (or emitted nothing).
        fail=1
        failed_lines+=("no TAP output")
    fi

    total_pass=$((total_pass + pass))
    total_fail=$((total_fail + fail))
    total_skip=$((total_skip + skip))

    if [ "${fail}" -ne 0 ]; then
        failed_tests+=("${name}")
        printf '[FAIL] %-40s pass=%-3d fail=%-3d skip=%-3d (log: %s)\n' \
            "${name}" "${pass}" "${fail}" "${skip}" "${log}"
        local failed
        for failed in "${failed_lines[@]}"; do
            printf '       %s\n' "${failed}"
        done
        echo "       --- last 40 lines of ${log} ---"
        tail -n 40 "${log}" | sed 's/^/       | /'
        echo "       ---"
    else
        printf '[PASS] %-40s pass=%-3d fail=%-3d skip=%-3d\n' \
            "${name}" "${pass}" "${fail}" "${skip}"
    fi
    printf '%s pass=%d fail=%d skip=%d rc=%d\n' "${name}" "${pass}" "${fail}" "${skip}" "${rc}" >>"${summary_file}"

    if [ "${keep}" = false ]; then
        rm -rf "${rundir}"
    else
        echo "       kept test tmpdir: ${rundir}"
    fi
}

echo "Running ${#selected[@]} test(s) with ${builddir}/ostree"
echo "  G_TEST_SRCDIR=${G_TEST_SRCDIR}"
echo "  G_TEST_BUILDDIR=${G_TEST_BUILDDIR}"
echo "  OSTREE_HTTPD=${OSTREE_HTTPD}"
echo

for name in "${selected[@]}"; do
    run_one "${name}"
done

echo
printf 'TAP summary: tests=%d pass=%d fail=%d skip=%d\n' \
    "${#selected[@]}" "${total_pass}" "${total_fail}" "${total_skip}"
echo "Per-test summary: ${summary_file}"
if [ "${#failed_tests[@]}" -gt 0 ]; then
    echo "Failed: ${failed_tests[*]}"
fi
if [ "${keep}" = true ]; then
    echo "Work directory kept: ${workdir}"
fi

if [ "${total_fail}" -ne 0 ]; then
    exit 1
fi
exit 0
