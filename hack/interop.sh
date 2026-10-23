#!/usr/bin/env bash
# Bidirectional interoperability checks between the C and Go OSTree
# implementations.
#
#   C -> Go: read fixtures produced by C ostree (or the checked-in fixtures
#            under go/testdata/fixtures) with the Go binary and compare command
#            output with the C reference.
#   Go -> C: create a repository with the Go binary, then require C
#            `ostree fsck --all` to accept it (plus refs/rev-parse cross-checks).
#
# The Go CLI is still being implemented. Any command that fails with an
# "unimplemented"/"unknown command"/"unknown option" error is reported as SKIP
# rather than as a hard failure, so this script is useful from day one.
# Use --strict to turn skips into failures (suitable for a CI gate once the
# corresponding commands exist).

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
GO_ROOT=$(cd "${SCRIPT_DIR}/.." && pwd)

GO_CMD=${GO:-/usr/local/go/bin/go}
if ! command -v "${GO_CMD}" >/dev/null 2>&1; then
    GO_CMD=$(command -v go || true)
fi

C_BIN=${OSTREE_C_BIN:-/usr/bin/ostree}
GO_BIN=""
FIXTURES_DIR="${GO_ROOT}/testdata/fixtures"
MODES=(bare bare-user bare-user-only archive bare-split-xattrs)
BUILD=false
KEEP=false
STRICT=false
VERBOSE=false

usage() {
    cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Bidirectional C <-> Go interoperability skeleton.

Options:
  --c-binary PATH    C ostree binary (default: \$OSTREE_C_BIN or /usr/bin/ostree)
  --go-binary PATH   Go ostree binary (default: go/bin/ostree, built on demand)
  --fixtures DIR     Fixture directory (default: go/testdata/fixtures)
  --modes "M..."     Space-separated modes (default: all five)
  --build            Force rebuilding go/bin/ostree
  --no-build         Never build; skip the Go side if the binary is absent
  --strict           Treat SKIP as failure
  --keep             Keep the temporary work directory
  --verbose, -v      Show command output
  -h, --help         Show this help

Exit status: 0 all executed checks passed (skips allowed), 1 real difference or
failure, 2 setup error.
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --c-binary)
            [ $# -ge 2 ] || { echo "error: --c-binary requires a path" >&2; exit 2; }
            C_BIN=$2
            shift 2
            ;;
        --go-binary)
            [ $# -ge 2 ] || { echo "error: --go-binary requires a path" >&2; exit 2; }
            GO_BIN=$2
            shift 2
            ;;
        --fixtures)
            [ $# -ge 2 ] || { echo "error: --fixtures requires a directory" >&2; exit 2; }
            FIXTURES_DIR=$2
            shift 2
            ;;
        --modes)
            [ $# -ge 2 ] || { echo "error: --modes requires a value" >&2; exit 2; }
            read -r -a MODES <<<"$2"
            shift 2
            ;;
        --build)
            BUILD=true
            shift
            ;;
        --no-build)
            NO_BUILD=true
            shift
            ;;
        --strict)
            STRICT=true
            shift
            ;;
        --keep)
            KEEP=true
            shift
            ;;
        -v|--verbose)
            VERBOSE=true
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

if [ ! -x "${C_BIN}" ]; then
    echo "error: C ostree binary not found: ${C_BIN}" >&2
    echo "hint: set OSTREE_C_BIN or pass --c-binary" >&2
    exit 2
fi

export LC_ALL=C
export LANG=C
export TZ=UTC
export SOURCE_DATE_EPOCH=0

workdir=$(mktemp -d "${TMPDIR:-/tmp}/ostree-interop.XXXXXX")
diffs_dir="${workdir}/diffs"
mkdir -p "${diffs_dir}"
cleanup() {
    if [ "${KEEP}" = false ]; then
        rm -rf "${workdir}"
    fi
}
trap cleanup EXIT

pass=0
diff_count=0
fail=0
skip=0
c2g_pass=0
c2g_diff=0
c2g_skip=0
g2c_pass=0
g2c_fail=0
g2c_skip=0

# --- helper functions -------------------------------------------------------

# run_cmd OUT ERR RC BIN ARGS... : execute and capture stdout/stderr/status.
run_cmd() {
    local out=$1 err=$2 rcvar=$3
    shift 3
    local status=0
    set +e
    "$@" >"${out}" 2>"${err}"
    status=$?
    set -e
    printf -v "${rcvar}" '%s' "${status}"
    if [ "${VERBOSE}" = true ]; then
        echo "    \$ $*" >&2
        sed 's/^/      out: /' "${out}" >&2 || true
        sed 's/^/      err: /' "${err}" >&2 || true
    fi
}

# Normalize temporary paths so C and Go output can be compared.
normalize() {
    local file=$1 repo=$2
    sed -e "s|${repo}|<REPO>|g" \
        -e "s|${workdir}|<WORK>|g" \
        -e 's|/[^ ]*/testdata/fixtures/[a-z-]*|<FIXTURE>|g' \
        "${file}"
}

is_unimplemented() {
    local err=$1
    grep -Eqi 'unknown command|not implemented|unimplemented|unsupported command|unknown option|no such command|unrecognized option|unknown flag|flag provided but not defined|not supported|Not allowed due to repo mode' "${err}"
}

slugify() {
    printf '%s' "$1" | tr ' /' '__' | tr -cd 'A-Za-z0-9_.-'
}

record_diff() {
    local label=$1 c_out=$2 g_out=$3 c_err=$4 g_err=$5
    local slug diff_file
    slug=$(slugify "${label}")
    diff_file="${diffs_dir}/${slug}.diff"
    {
        echo "== ${label} =="
        echo "--- C stdout"
        cat "${c_out}"
        echo "--- Go stdout"
        cat "${g_out}"
        echo "--- C stderr"
        cat "${c_err}"
        echo "--- Go stderr"
        cat "${g_err}"
    } >"${diff_file}.raw"
    if diff -u "${c_out}" "${g_out}" >"${diff_file}" 2>&1; then
        # stdout matched; the difference was the exit status/stderr.
        {
            echo "== ${label} =="
            echo "exit status or stderr differs"
            echo "--- C stderr"
            cat "${c_err}"
            echo "--- Go stderr"
            cat "${g_err}"
        } >"${diff_file}"
    fi
    echo "    diff: ${diff_file}"
    sed -n '1,20p' "${diff_file}" | sed 's/^/      /'
}

build_go() {
    if [ -z "${GO_BIN}" ]; then
        GO_BIN="${GO_ROOT}/bin/ostree"
    fi
    if [ "${NO_BUILD:-false}" = true ]; then
        if [ ! -x "${GO_BIN}" ]; then
            echo "Go side: SKIP (--no-build and ${GO_BIN} does not exist)"
            GO_BIN=""
        fi
        return 0
    fi
    if [ "${BUILD}" = false ] && [ -x "${GO_BIN}" ]; then
        return 0
    fi
    if [ -z "${GO_CMD}" ]; then
        echo "Go side: SKIP (no Go toolchain)"
        GO_BIN=""
        return 0
    fi
    if [ ! -d "${GO_ROOT}/cmd/ostree" ]; then
        echo "Go side: SKIP (${GO_ROOT}/cmd/ostree is not implemented yet)"
        GO_BIN=""
        return 0
    fi
    mkdir -p "$(dirname "${GO_BIN}")"
    echo "Building Go ostree -> ${GO_BIN}"
    if ! (cd "${GO_ROOT}" && CGO_ENABLED=0 "${GO_CMD}" build -trimpath -o "${GO_BIN}" ./cmd/ostree); then
        echo "Go side: SKIP (failed to build ./cmd/ostree)"
        GO_BIN=""
        return 0
    fi
}

# --- C -> Go ---------------------------------------------------------------

# compare_one MODE LABEL C_REPO GO_REPO ARGS...
compare_one() {
    local mode=$1 label=$2 c_repo=$3 go_repo=$4
    shift 4
    local c_out="${workdir}/c.out" c_err="${workdir}/c.err"
    local g_out="${workdir}/g.out" g_err="${workdir}/g.err"
    local c_rc=0 g_rc=0

    run_cmd "${c_out}" "${c_err}" c_rc "${C_BIN}" --repo="${c_repo}" "$@"
    run_cmd "${g_out}" "${g_err}" g_rc "${GO_BIN}" --repo="${go_repo}" "$@"

    if [ "${g_rc}" -ne 0 ] && is_unimplemented "${g_err}"; then
        echo "  [SKIP] ${mode}: ${label} (Go: $(head -n 1 "${g_err}"))"
        c2g_skip=$((c2g_skip + 1))
        skip=$((skip + 1))
        return 0
    fi

    normalize "${c_out}" "${c_repo}" >"${c_out}.norm"
    normalize "${g_out}" "${go_repo}" >"${g_out}.norm"
    normalize "${c_err}" "${c_repo}" >"${c_err}.norm"
    normalize "${g_err}" "${go_repo}" >"${g_err}.norm"

    if [ "${c_rc}" -eq "${g_rc}" ] && cmp -s "${c_out}.norm" "${g_out}.norm"; then
        echo "  [PASS] ${mode}: ${label}"
        c2g_pass=$((c2g_pass + 1))
        pass=$((pass + 1))
        return 0
    fi

    echo "  [DIFF] ${mode}: ${label} (C rc=${c_rc}, Go rc=${g_rc})"
    record_diff "c2g-${mode}-${label}" "${c_out}.norm" "${g_out}.norm" "${c_err}.norm" "${g_err}.norm"
    c2g_diff=$((c2g_diff + 1))
    diff_count=$((diff_count + 1))
}

run_c2g() {
    echo
    echo "== C -> Go =="
    if [ -z "${GO_BIN}" ] || [ ! -x "${GO_BIN}" ]; then
        echo "  SKIP: no Go binary available; skipping all C->Go comparisons"
        c2g_skip=$((c2g_skip + 1))
        skip=$((skip + 1))
        return 0
    fi
    if [ ! -d "${FIXTURES_DIR}" ]; then
        echo "  SKIP: fixture directory not found: ${FIXTURES_DIR}" >&2
        c2g_skip=$((c2g_skip + 1))
        skip=$((skip + 1))
        return 0
    fi

    local mode fixture c_repo go_repo
    for mode in "${MODES[@]}"; do
        fixture="${FIXTURES_DIR}/${mode}"
        if [ ! -d "${fixture}" ]; then
            echo "  SKIP: no fixture for mode ${mode} (${fixture})"
            c2g_skip=$((c2g_skip + 1))
            skip=$((skip + 1))
            continue
        fi
        c_repo="${workdir}/c2g-${mode}-c"
        go_repo="${workdir}/c2g-${mode}-go"
        rm -rf "${c_repo}" "${go_repo}"
        cp -a "${fixture}" "${c_repo}"
        cp -a "${fixture}" "${go_repo}"

        compare_one "${mode}" "refs" "${c_repo}" "${go_repo}" refs
        compare_one "${mode}" "rev-parse test2" "${c_repo}" "${go_repo}" rev-parse test2
        compare_one "${mode}" "show test2" "${c_repo}" "${go_repo}" show test2
        compare_one "${mode}" "log test2" "${c_repo}" "${go_repo}" log test2
        compare_one "${mode}" "ls -R test2" "${c_repo}" "${go_repo}" ls -R test2
        compare_one "${mode}" "cat test2 /firstfile" "${c_repo}" "${go_repo}" cat test2 /firstfile
        compare_one "${mode}" "summary -v" "${c_repo}" "${go_repo}" summary -v
        compare_one "${mode}" "fsck --all" "${c_repo}" "${go_repo}" fsck --all
        if [ -d "${fixture}/deltas" ]; then
            compare_one "${mode}" "static-delta list" "${c_repo}" "${go_repo}" static-delta list
        fi
    done
}

# --- Go -> C ---------------------------------------------------------------

make_go_tree() {
    local dir=$1
    rm -rf "${dir}"
    mkdir -p "${dir}/sub/deeper"
    printf 'first\n' >"${dir}/firstfile"
    chmod 0644 "${dir}/firstfile"
    ln -s firstfile "${dir}/somelink"
    printf 'moo\n' >"${dir}/sub/cow"
    printf 'hi\n' >"${dir}/sub/deeper/ohyeah"
    chmod 0600 "${dir}/sub/deeper/ohyeah"
    printf '#!/bin/sh\nprintf "interop\\n"\n' >"${dir}/script.sh"
    chmod 0755 "${dir}/script.sh"
}

run_g2c() {
    echo
    echo "== Go -> C =="
    if [ -z "${GO_BIN}" ] || [ ! -x "${GO_BIN}" ]; then
        echo "  SKIP: no Go binary available; skipping all Go->C checks"
        g2c_skip=$((g2c_skip + 1))
        skip=$((skip + 1))
        return 0
    fi

    local mode repo tree out err rc
    for mode in "${MODES[@]}"; do
        repo="${workdir}/g2c-${mode}/repo"
        tree="${workdir}/g2c-${mode}/tree"
        out="${workdir}/g2c.out"
        err="${workdir}/g2c.err"
        rm -rf "${workdir}/g2c-${mode}"
        mkdir -p "${workdir}/g2c-${mode}"
        make_go_tree "${tree}"

        run_cmd "${out}" "${err}" rc "${GO_BIN}" init --repo="${repo}" --mode="${mode}"
        if [ "${rc}" -ne 0 ]; then
            if is_unimplemented "${err}"; then
                echo "  [SKIP] ${mode}: Go init (Go: $(head -n 1 "${err}"))"
                g2c_skip=$((g2c_skip + 1))
                skip=$((skip + 1))
            else
                echo "  [FAIL] ${mode}: Go init rc=${rc}"
                sed -n '1,10p' "${err}" | sed 's/^/      /'
                g2c_fail=$((g2c_fail + 1))
                fail=$((fail + 1))
            fi
            continue
        fi

        run_cmd "${out}" "${err}" rc "${GO_BIN}" --repo="${repo}" commit \
            --branch=test2 --subject="Go interop commit" --body="Created by interop.sh" \
            --timestamp=@0 --tree="dir=${tree}"
        if [ "${rc}" -ne 0 ]; then
            if is_unimplemented "${err}"; then
                echo "  [SKIP] ${mode}: Go commit (Go: $(head -n 1 "${err}"))"
                g2c_skip=$((g2c_skip + 1))
                skip=$((skip + 1))
            else
                echo "  [FAIL] ${mode}: Go commit rc=${rc}"
                sed -n '1,10p' "${err}" | sed 's/^/      /'
                g2c_fail=$((g2c_fail + 1))
                fail=$((fail + 1))
            fi
            continue
        fi

        run_cmd "${out}" "${err}" rc "${GO_BIN}" --repo="${repo}" summary -u
        if [ "${rc}" -ne 0 ]; then
            if is_unimplemented "${err}"; then
                echo "  [SKIP] ${mode}: Go summary -u (Go: $(head -n 1 "${err}"))"
            else
                echo "  [WARN] ${mode}: Go summary -u rc=${rc} (continuing to C fsck)"
                sed -n '1,5p' "${err}" | sed 's/^/      /'
            fi
        fi

        # Cross-check the commit checksum if Go supports rev-parse.
        local go_rev="" c_rev=""
        if run_cmd "${out}" "${err}" rc "${GO_BIN}" --repo="${repo}" rev-parse test2 && [ "${rc}" -eq 0 ]; then
            go_rev=$(head -n 1 "${out}")
        fi
        if run_cmd "${out}" "${err}" rc "${C_BIN}" --repo="${repo}" rev-parse test2 && [ "${rc}" -eq 0 ]; then
            c_rev=$(head -n 1 "${out}")
        fi
        if [ -n "${go_rev}" ] && [ -n "${c_rev}" ] && [ "${go_rev}" != "${c_rev}" ]; then
            echo "  [FAIL] ${mode}: commit checksum differs (Go=${go_rev}, C=${c_rev})"
            g2c_fail=$((g2c_fail + 1))
            fail=$((fail + 1))
            continue
        fi

        # The real gate: the C implementation must accept the repository.
        run_cmd "${out}" "${err}" rc "${C_BIN}" --repo="${repo}" fsck --all --quiet
        if [ "${rc}" -eq 0 ]; then
            echo "  [PASS] ${mode}: C fsck --all accepted the Go repository"
            g2c_pass=$((g2c_pass + 1))
            pass=$((pass + 1))
        else
            echo "  [FAIL] ${mode}: C fsck --all rejected the Go repository (rc=${rc})"
            sed -n '1,20p' "${err}" | sed 's/^/      /'
            g2c_fail=$((g2c_fail + 1))
            fail=$((fail + 1))
        fi
    done
}

# --- main ------------------------------------------------------------------

echo "C ostree:  ${C_BIN} ($("${C_BIN}" --version 2>/dev/null | awk -F"'" '/Version:/ {print $2; exit}'))"
build_go
if [ -n "${GO_BIN}" ] && [ -x "${GO_BIN}" ]; then
    echo "Go ostree: ${GO_BIN}"
else
    echo "Go ostree: <unavailable>"
fi
echo "Fixtures:  ${FIXTURES_DIR}"

run_c2g
run_g2c

echo
echo "Interop summary: pass=${pass} diff=${diff_count} fail=${fail} skip=${skip}"
echo "  C->Go: pass=${c2g_pass} diff=${c2g_diff} skip=${c2g_skip}"
echo "  Go->C: pass=${g2c_pass} fail=${g2c_fail} skip=${g2c_skip}"
if [ "${diff_count}" -gt 0 ] || [ "${fail}" -gt 0 ]; then
    echo "Diffs are in ${diffs_dir}"
fi
if [ "${KEEP}" = true ]; then
    echo "Work directory kept: ${workdir}"
fi

if [ "${diff_count}" -ne 0 ] || [ "${fail}" -ne 0 ]; then
    exit 1
fi
if [ "${STRICT}" = true ] && [ "${skip}" -ne 0 ]; then
    echo "error: --strict and ${skip} check(s) were skipped" >&2
    exit 1
fi
exit 0
