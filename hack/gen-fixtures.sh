#!/usr/bin/env bash
# Generate deterministic C-ostree fixtures for the Go implementation.
#
# For each repository mode (bare, bare-user, bare-user-only, archive,
# bare-split-xattrs) this script creates a small repository with two commits,
# two refs and a summary. The archive fixture additionally contains one static
# delta. Only the read-only parts of each repository are copied:
#
#   objects/  refs/  config  summary  [deltas/]
#
# Each fixture directory also gets a SHA256SUMS file covering that payload and
# a META file recording the C ostree version, the exact ostree commands and the
# generation time.
#
# Reproducibility:
#   * commits use --timestamp=@0 and fixed owner uid/gid;
#   * LC_ALL=C, TZ=UTC and a fixed umask are exported;
#   * a tiny LD_PRELOAD shim pins the GLib wall clock while the summary and
#     static delta are written, so `ostree.summary.last-modified` and the delta
#     superblock timestamp are deterministic. It overrides g_get_real_time()
#     plus g_date_time_new_now{,_utc,_local}(), which the delta code uses
#     directly. (The C library has no supported time override knob.)
#   * SHA256SUMS therefore stays byte-identical across reruns. META records the
#     wall-clock time of the run and is intentionally not covered by SHA256SUMS.
#
# The script is idempotent: staging directories are recreated and each fixture
# is replaced atomically-ish (rm -rf + copy) on every run.

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
GO_ROOT=$(cd "${SCRIPT_DIR}/.." && pwd)
REPO_ROOT=$(cd "${GO_ROOT}/.." && pwd)

OSTREE_BIN=${OSTREE_BIN:-/usr/bin/ostree}
OUT_DIR=${OUT_DIR:-"${GO_ROOT}/testdata/fixtures"}
EXPECTED_VERSION=2025.2
MODES=(bare bare-user bare-user-only archive bare-split-xattrs)
SPLIT_TAR="${REPO_ROOT}/tests/bare-split-xattrs-basic.tar.xz"
OWNER_UID=${OWNER_UID:-1000}
OWNER_GID=${OWNER_GID:-1000}
FIXTURE_EPOCH=0
USE_FAKE_TIME=true
USE_XATTRS=true
KEEP_WORKDIR=false

usage() {
    cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Generate C-ostree repositories under go/testdata/fixtures.

Options:
  --ostree PATH        C ostree binary (default: /usr/bin/ostree)
  --out DIR            Output fixture directory (default: go/testdata/fixtures)
  --modes "M..."       Space-separated modes (default: all five)
  --no-fake-time       Do not pin summary mtime (fixtures become non-reproducible)
  --no-xattrs          Do not set/import user xattrs (bare-user-only always does)
  --keep               Keep the temporary work directory
  -h, --help           Show this help

Environment: OSTREE_BIN, OUT_DIR
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --ostree)
            [ $# -ge 2 ] || { echo "error: --ostree requires a path" >&2; exit 2; }
            OSTREE_BIN=$2
            shift 2
            ;;
        --out)
            [ $# -ge 2 ] || { echo "error: --out requires a directory" >&2; exit 2; }
            OUT_DIR=$2
            shift 2
            ;;
        --modes)
            [ $# -ge 2 ] || { echo "error: --modes requires a value" >&2; exit 2; }
            read -r -a MODES <<<"$2"
            shift 2
            ;;
        --no-fake-time)
            USE_FAKE_TIME=false
            shift
            ;;
        --no-xattrs)
            USE_XATTRS=false
            shift
            ;;
        --keep)
            KEEP_WORKDIR=true
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

if [ ! -x "${OSTREE_BIN}" ]; then
    echo "error: C ostree binary not found or not executable: ${OSTREE_BIN}" >&2
    echo "hint: pass --ostree /usr/bin/ostree" >&2
    exit 2
fi

if [ "${USE_XATTRS}" = true ]; then
    command -v python3 >/dev/null 2>&1 || {
        echo "error: python3 is required to set deterministic user xattrs" >&2
        echo "hint: rerun with --no-xattrs if the fixtures do not need xattrs" >&2
        exit 2
    }
fi

export LC_ALL=C
export LANG=C
export TZ=UTC
export SOURCE_DATE_EPOCH="${FIXTURE_EPOCH}"
# Required by ostree for writing bare-split-xattrs repositories.
export OSTREE_EXP_WRITE_BARE_SPLIT_XATTRS=1
umask 022

workdir=$(mktemp -d "${TMPDIR:-/tmp}/ostree-fixtures.XXXXXX")
cleanup() {
    if [ -n "${OUT_DIR:-}" ]; then
        rm -rf "${OUT_DIR}/.stage-"*
    fi
    if [ "${KEEP_WORKDIR}" = false ]; then
        rm -rf "${workdir}"
    fi
}
trap cleanup EXIT

# Build the LD_PRELOAD shim used to pin g_get_real_time() during summary
# generation. No GLib headers are needed: the symbol is a plain int64 return.
time_shim=""
if [ "${USE_FAKE_TIME}" = true ]; then
    if command -v cc >/dev/null 2>&1; then
        cat >"${workdir}/no-clock.c" <<'CEOF'
/* Pin GLib's wall clock to the Unix epoch for reproducible summaries and
 * static-delta superblocks.  g_get_real_time() covers the summary metadata;
 * the g_date_time_new_now*() overrides cover the delta superblock timestamp,
 * whose implementation does not route through the interposable
 * g_get_real_time() symbol. */
typedef long long gint64;
gint64 g_get_real_time (void) { return 0; }

typedef struct _GDateTime GDateTime;
extern GDateTime *g_date_time_new_from_unix_utc (gint64 t);
extern GDateTime *g_date_time_new_from_unix_local (gint64 t);
GDateTime *g_date_time_new_now_utc (void) { return g_date_time_new_from_unix_utc (0); }
GDateTime *g_date_time_new_now_local (void) { return g_date_time_new_from_unix_local (0); }
GDateTime *g_date_time_new_now (void) { return g_date_time_new_from_unix_local (0); }
CEOF
        if cc -shared -fPIC -o "${workdir}/no-clock.so" "${workdir}/no-clock.c" 2>"${workdir}/cc.log"; then
            time_shim="${workdir}/no-clock.so"
        else
            echo "warning: failed to build time shim; summary timestamps will vary" >&2
            sed 's/^/  /' "${workdir}/cc.log" >&2
        fi
    else
        echo "warning: no C compiler; summary timestamps will vary" >&2
    fi
fi

ostree_version=$("${OSTREE_BIN}" --version)
version_line=$(printf '%s\n' "${ostree_version}" | awk -F"'" '/Version:/ { print $2; exit }')
if [ "${version_line}" != "${EXPECTED_VERSION}" ]; then
    echo "warning: expected C ostree ${EXPECTED_VERSION}, found ${version_line:-unknown}" >&2
fi

commands_file="${workdir}/commands.txt"
: >"${commands_file}"

# Record and execute a command.
run() {
    printf '%s\n' "$(printf '%q ' "$@")" >>"${commands_file}"
    "$@"
}

# Record and execute a command, capturing stdout.
run_capture() {
    printf '%s\n' "$(printf '%q ' "$@")" >>"${commands_file}"
    "$@"
}

# summary --update must run through the time shim when available.
run_summary() {
    local repo=$1
    if [ -n "${time_shim}" ]; then
        printf 'LD_PRELOAD=<epoch-shim> %q --repo=%q summary --update\n' \
            "${OSTREE_BIN}" "${repo}" >>"${commands_file}"
        LD_PRELOAD="${time_shim}" "${OSTREE_BIN}" --repo="${repo}" summary --update
    else
        run "${OSTREE_BIN}" --repo="${repo}" summary --update
    fi
}

# Static delta superblocks embed a wall-clock timestamp as well.
run_delta_generate() {
    local repo=$1
    local from=$2
    local to=$3
    if [ -n "${time_shim}" ]; then
        printf 'LD_PRELOAD=<epoch-shim> %q --repo=%q static-delta generate --from=%q --to=%q\n' \
            "${OSTREE_BIN}" "${repo}" "${from}" "${to}" >>"${commands_file}"
        LD_PRELOAD="${time_shim}" "${OSTREE_BIN}" --repo="${repo}" \
            static-delta generate --from="${from}" --to="${to}"
    else
        run "${OSTREE_BIN}" --repo="${repo}" static-delta generate \
            --from="${from}" --to="${to}"
    fi
}

# Populate the canonical work tree. Content must stay stable: it is what the
# commit checksums are derived from.
make_content_tree() {
    local dir=$1
    rm -rf "${dir}"
    mkdir -p "${dir}"
    (
        cd "${dir}"
        mkdir -p baz/deeper baz/another baz/sub1 baz/sub2 empty-dir

        printf 'first\n' >firstfile
        chmod 0644 firstfile

        ln -s firstfile somelink
        ln -s nonexistent baz/alink

        printf 'moo\n' >baz/cow
        printf 'mooro\n' >baz/cowro
        chmod 0600 baz/cowro
        printf 'alien\n' >baz/saucer
        printf 'hi\n' >baz/deeper/ohyeah
        printf 'hix\n' >baz/deeper/ohyeahx
        chmod 0755 baz/deeper/ohyeahx
        printf 'x\n' >baz/another/y

        printf 'SAME_CONTENT\n' >baz/sub1/duplicate_a
        printf 'SAME_CONTENT\n' >baz/sub1/duplicate_b
        printf 'SAME_CONTENT\n' >baz/sub2/duplicate_c

        printf '#!/bin/sh\nprintf "fixture\\n"\n' >baz/script.sh
        chmod 0755 baz/script.sh
        : >empty-file
    )

    if [ "${USE_XATTRS}" = true ]; then
        python3 - "${dir}/firstfile" <<'PYEOF'
import os
import sys

path = sys.argv[1]
try:
    os.setxattr(path, b"user.ostree.fixture", b"fixture-value")
    os.setxattr(path, b"user.ostree.other", b"second")
except OSError as exc:
    sys.stderr.write(
        "error: cannot set user xattrs on %s: %s\n"
        "hint: rerun with --no-xattrs, or use a filesystem with user xattr support\n"
        % (path, exc)
    )
    sys.exit(1)
PYEOF
    fi
}

commit_args_for_mode() {
    local mode=$1
    case "${mode}" in
        bare-user-only)
            # ostree requires canonical ownership/permissions and no xattrs.
            printf '%s\n' --owner-uid=0 --owner-gid=0 --no-xattrs --canonical-permissions
            ;;
        *)
            printf '%s\n' "--owner-uid=${OWNER_UID}" "--owner-gid=${OWNER_GID}"
            if [ "${USE_XATTRS}" = false ]; then
                printf '%s\n' --no-xattrs
            fi
            ;;
    esac
}

write_meta() {
    local dest=$1
    local mode=$2
    local rev1=$3
    local rev2=$4
    local source_note=$5
    local generated_at
    local xattr_note
    local meta_uid="${OWNER_UID}"
    local meta_gid="${OWNER_GID}"

    if [ "${mode}" = bare-user-only ]; then
        meta_uid=0
        meta_gid=0
    elif [ "${mode}" = bare-split-xattrs ] && [ "${source_note#upstream}" != "${source_note}" ]; then
        meta_uid=0
        meta_gid=0
    fi

    if [ "${mode}" = bare-user-only ]; then
        xattr_note="disabled (required by the mode)"
    elif [ "${mode}" = bare-split-xattrs ] && [ "${source_note#upstream}" != "${source_note}" ]; then
        xattr_note="from upstream fixture (user.mykey on /bar)"
    else
        xattr_note="${USE_XATTRS}"
    fi

    generated_at=${FIXTURE_GENERATED_AT:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
    {
        echo "fixture_format=1"
        echo "mode=${mode}"
        echo "generated_at_utc=${generated_at}"
        echo "fixture_epoch=${FIXTURE_EPOCH}"
        echo "ostree_binary=${OSTREE_BIN}"
        echo "ostree_version=${version_line:-unknown}"
        echo "owner_uid=${meta_uid}"
        echo "owner_gid=${meta_gid}"
        echo "commit1=${rev1}"
        echo "commit2=${rev2}"
        echo "xattrs=${xattr_note}"
        echo "fake_time=$([ -n "${time_shim}" ] && echo yes || echo no)"
        echo "source=${source_note}"
        echo "payload=objects refs config summary$([ -d "${dest}/deltas" ] && echo ' deltas')"
        echo "sha256sums=regular files only; symlink object targets are listed below"
        echo
        echo "[ostree --version]"
        printf '%s\n' "${ostree_version}" | sed 's/^/  /'
        echo
        echo "[ostree commands]"
        sed -e "s#${workdir}#<TMP>#g" \
            -e 's#--tree=dir=[^ ]*#--tree=dir=<TREE>#g' \
            -e 's#<TMP>/[^ ]*/repo#<REPO>#g' \
            "${commands_file}" | sed 's/^/  /'
        echo
        echo "[symlinks]"
        if ( cd "${dest}" && find objects refs -type l -print -quit 2>/dev/null | grep -q . ); then
            ( cd "${dest}" && find objects refs -type l -printf '  %P -> %l\n' | LC_ALL=C sort )
        else
            echo "  (none)"
        fi
        echo
        echo "[contents]"
        if [ "${mode}" = bare-split-xattrs ] && [ "${source_note#upstream}" != "${source_note}" ]; then
            cat <<'EOF'
  /foo                       regular file, 0644, no xattrs
  /bar                       regular file, 0644, user.mykey="myvalue"
  /foolink -> foo            symlink
EOF
        else
            cat <<'EOF'
  firstfile                  regular file, 0644, user xattrs (unless --no-xattrs)
  somelink -> firstfile      symlink
  baz/cow                    regular file, 0644
  baz/cowro                  regular file, 0600
  baz/deeper/ohyeahx         regular file, 0755
  baz/alink -> nonexistent   dangling symlink
  baz/script.sh              executable, 0755
  baz/sub1/duplicate_a       same content as duplicate_b
  baz/sub1/duplicate_b       same content as duplicate_a
  baz/sub2/duplicate_c       same content as duplicate_a
  empty-file                 zero-length regular file
  empty-dir/                 empty directory
EOF
        fi
    } >"${dest}/META"
}

write_sha256sums() {
    local dest=$1
    local list
    list=$(mktemp "${workdir}/filelist.XXXXXX")
    (
        cd "${dest}"
        {
            find objects refs -type f 2>/dev/null || true
            [ -f config ] && echo config
            [ -f summary ] && echo summary
            if [ -d deltas ]; then
                find deltas -type f
            fi
        } | LC_ALL=C sort -u >"${list}"
        # shellcheck disable=SC2046
        xargs -d '\n' sha256sum -- <"${list}" >SHA256SUMS
    )
    rm -f "${list}"
}

validate_repo() {
    local repo=$1
    local mode=$2
    local valdir="${workdir}/validate-${mode}"
    rm -rf "${valdir}"
    mkdir -p "${valdir}"
    cp -a "${repo}/." "${valdir}/"
    if ! "${OSTREE_BIN}" --repo="${valdir}" fsck --all --quiet >"${workdir}/fsck-${mode}.log" 2>&1; then
        echo "error: fsck --all failed for staged ${mode} fixture" >&2
        sed 's/^/  /' "${workdir}/fsck-${mode}.log" >&2
        return 1
    fi
    if [ "${mode}" = archive ]; then
        if ! find "${repo}/deltas" -type f -print -quit | grep -q .; then
            echo "error: archive fixture has no static delta files under deltas/" >&2
            return 1
        fi
        if ! "${OSTREE_BIN}" --repo="${valdir}" static-delta list >"${workdir}/deltas-${mode}.txt" 2>&1; then
            echo "error: static-delta list failed for staged archive fixture" >&2
            sed 's/^/  /' "${workdir}/deltas-${mode}.txt" >&2
            return 1
        fi
    fi
    rm -rf "${valdir}"
}

mkdir -p "${OUT_DIR}"
OUT_DIR=$(cd "${OUT_DIR}" && pwd)

declare -A fixture_commits=()

for mode in "${MODES[@]}"; do
    echo "=== ${mode} ==="
    repo="${workdir}/${mode}/repo"
    tree="${workdir}/${mode}/files"
    stage="${OUT_DIR}/.stage-${mode}"
    dest="${OUT_DIR}/${mode}"
    rm -rf "${repo}" "${stage}"
    mkdir -p "${repo}"
    : >"${commands_file}"

    make_content_tree "${tree}"

    mapfile -t commit_args < <(commit_args_for_mode "${mode}")

    rev1=""
    rev2=""
    source_note="generated by ${OSTREE_BIN}"

    if [ "${mode}" = bare-split-xattrs ]; then
        # Upstream C ostree 2025.2 only supports *writing* this mode behind
        # OSTREE_EXP_WRITE_BARE_SPLIT_XATTRS, and even then the resulting repo
        # is incomplete (upstream test-basic-bare-split-xattrs.sh asserts that
        # fsck fails). Try the experimental writer first; if fsck rejects the
        # result, fall back to the C-generated read-only fixture shipped by
        # upstream.
        main_commands="${commands_file}"
        commands_file="${workdir}/split-attempt-commands.txt"
        : >"${commands_file}"
        split_ok=false
        run "${OSTREE_BIN}" --repo="${repo}" init --mode="${mode}" || true
        if rev1=$(run_capture "${OSTREE_BIN}" --repo="${repo}" commit "${commit_args[@]}" \
                --timestamp="@${FIXTURE_EPOCH}" -b test2 \
                -s "Fixture commit 1" -m "Commit body first" --tree="dir=${tree}" 2>"${workdir}/split-commit.log"); then
            printf 'second\n' >"${tree}/secondfile"
            if rev2=$(run_capture "${OSTREE_BIN}" --repo="${repo}" commit "${commit_args[@]}" \
                    --timestamp="@${FIXTURE_EPOCH}" -b test2 \
                    -s "Fixture commit 2" -m "Commit body second" --tree="dir=${tree}" 2>>"${workdir}/split-commit.log"); then
                run "${OSTREE_BIN}" --repo="${repo}" refs --create=stable "${rev2}"
                run_summary "${repo}"
                if run "${OSTREE_BIN}" --repo="${repo}" fsck --all --quiet >"${workdir}/fsck-split.log" 2>&1; then
                    split_ok=true
                fi
            fi
        fi
        if [ "${split_ok}" = true ]; then
            cat "${commands_file}" >>"${main_commands}"
            commands_file="${main_commands}"
        else
            # The experimental write attempt did not produce a usable repo;
            # do not claim its commands in META.
            commands_file="${main_commands}"
            echo "  warning: C ostree cannot write a valid bare-split-xattrs repo;" >&2
            echo "           falling back to ${SPLIT_TAR#${REPO_ROOT}/}" >&2
            if [ ! -f "${SPLIT_TAR}" ]; then
                echo "error: fallback fixture not found: ${SPLIT_TAR}" >&2
                exit 1
            fi
            rm -rf "${repo}" "${workdir}/split-upstream"
            mkdir -p "${workdir}/split-upstream"
            run tar --same-permissions --same-owner -xaf "${SPLIT_TAR}" \
                -C "${workdir}/split-upstream"
            repo="${workdir}/split-upstream/repo"
            split_commit_path=$(find "${repo}/objects" -type f -name '*.commit' -print -quit)
            if [ -z "${split_commit_path}" ]; then
                echo "error: no commit object in ${SPLIT_TAR}" >&2
                exit 1
            fi
            split_dir=$(basename "$(dirname "${split_commit_path}")")
            split_base=$(basename "${split_commit_path}" .commit)
            rev1="${split_dir}${split_base}"
            rev2="${rev1}"
            run "${OSTREE_BIN}" --repo="${repo}" refs --create=test2 "${rev1}"
            run "${OSTREE_BIN}" --repo="${repo}" refs --create=stable "${rev1}"
            run_summary "${repo}"
            source_note="upstream ${SPLIT_TAR#${REPO_ROOT}/} (C ${version_line:-unknown} cannot write this mode)"
            if ! run "${OSTREE_BIN}" --repo="${repo}" fsck --all --quiet >"${workdir}/fsck-split.log" 2>&1; then
                echo "error: upstream bare-split-xattrs fixture failed fsck" >&2
                sed 's/^/  /' "${workdir}/fsck-split.log" >&2
                exit 1
            fi
        fi
    else
        run "${OSTREE_BIN}" --repo="${repo}" init --mode="${mode}"

        rev1=$(run_capture "${OSTREE_BIN}" --repo="${repo}" commit "${commit_args[@]}" \
            --timestamp="@${FIXTURE_EPOCH}" -b test2 \
            -s "Fixture commit 1" -m "Commit body first" --tree="dir=${tree}")

        printf 'second\n' >"${tree}/secondfile"
        rev2=$(run_capture "${OSTREE_BIN}" --repo="${repo}" commit "${commit_args[@]}" \
            --timestamp="@${FIXTURE_EPOCH}" -b test2 \
            -s "Fixture commit 2" -m "Commit body second" --tree="dir=${tree}")

        # A second ref makes ref iteration / summary handling more interesting.
        run "${OSTREE_BIN}" --repo="${repo}" refs --create=stable "${rev2}"

        run_summary "${repo}"

        if [ "${mode}" = archive ]; then
            run_delta_generate "${repo}" "${rev1}" "${rev2}"
            run_summary "${repo}"
        fi
    fi

    run "${OSTREE_BIN}" --repo="${repo}" fsck --all --quiet >"${workdir}/fsck-${mode}.log" 2>&1 || {
        echo "error: fsck --all failed for generated ${mode} repository" >&2
        sed 's/^/  /' "${workdir}/fsck-${mode}.log" >&2
        exit 1
    }

    mkdir -p "${stage}"
    cp -a "${repo}/objects" "${repo}/refs" "${repo}/config" "${repo}/summary" "${stage}/"
    if [ -d "${repo}/deltas" ]; then
        cp -a "${repo}/deltas" "${stage}/"
    fi

    validate_repo "${stage}" "${mode}" || exit 1
    write_sha256sums "${stage}"
    write_meta "${stage}" "${mode}" "${rev1}" "${rev2}" "${source_note}"

    rm -rf "${dest}"
    mv "${stage}" "${dest}"

    fixture_commits["${mode}"]="${rev2}"
    printf '  commit1=%s\n  commit2=%s\n  refs=test2,stable\n' "${rev1}" "${rev2}"
done

echo
echo "Fixtures written to ${OUT_DIR}:"
for mode in "${MODES[@]}"; do
    printf '  %-20s %s\n' "${mode}" "${fixture_commits[${mode}]:-MISSING}"
done
echo
echo "Verify with:"
echo "  (cd ${OUT_DIR}/<mode> && sha256sum -c SHA256SUMS)"
echo "  ${OSTREE_BIN} --repo=${OUT_DIR}/archive refs"
if [ -n "${time_shim}" ]; then
    echo
    echo "Summary timestamps were pinned with an LD_PRELOAD shim (reproducible payload)."
fi
if [ "${KEEP_WORKDIR}" = true ]; then
    echo "Work directory kept: ${workdir}"
fi
