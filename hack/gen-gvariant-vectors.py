#!/usr/bin/env python3
"""Generate authoritative GVariant test vectors for the pure-Go OSTree codec.

This script uses the system PyGObject bindings to GLib (``python3-gi``) as the
reference implementation.  For every vector it stores:

  * the GVariant type string,
  * a JSON-native logical value,
  * the canonical GLib GVariant text representation,
  * the serialized bytes (hex) and their length.

It also copies real OSTree ``.commit``, ``.dirtree`` and ``.dirmeta`` objects
from ``/tmp/ostree-probe`` (or creates a tiny deterministic bare repository
with ``ostree`` when the probe does not exist) and records their type,
checksum and a decoded logical value.

Outputs (and *only* these outputs are touched):

  go/internal/gvariant/testdata/vectors.json
  go/internal/gvariant/testdata/ostree-objects/<checksum>.<kind>

The serialized GVariant framing produced by GLib is host-endian; this script
asserts a little-endian host so the checked-in JSON is stable.  Real OSTree
objects are different: their GVariant framing is host-endian but OSTree stores
all fixed-width scalar *payloads* in big-endian order (see the comments above
OSTREE_*_GVARIANT_STRING in src/libostree/ostree-core.h).  Object entries
therefore carry ``"scalar_endianness": "big"``.

Run from anywhere:

    python3 go/hack/gen-gvariant-vectors.py

The script is idempotent: it rewrites vectors.json atomically and recreates
ostree-objects/ from scratch on every run.
"""

from __future__ import annotations

import hashlib
import json
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path
from typing import Any

try:
    import gi

    gi.require_version("GLib", "2.0")
    from gi.repository import GLib
except (ImportError, ValueError) as exc:  # pragma: no cover - environment issue
    sys.exit(
        "error: python3-gi with GLib introspection is required "
        f"(python3 -c 'import gi' failed: {exc})"
    )


# ---------------------------------------------------------------------------
# Paths and constants
# ---------------------------------------------------------------------------

SCRIPT_PATH = Path(__file__).resolve()
GO_ROOT = SCRIPT_PATH.parent.parent  # .../go
TESTDATA_DIR = GO_ROOT / "internal" / "gvariant" / "testdata"
VECTORS_PATH = TESTDATA_DIR / "vectors.json"
OBJECTS_DIR = TESTDATA_DIR / "ostree-objects"

PROBE_ROOT = Path("/tmp/ostree-probe")
OSTREE = shutil.which("ostree") or "/usr/bin/ostree"

# Object kind -> GVariant type (src/libostree/ostree-core.h).
OBJECT_TYPES = {
    "commit": "(a{sv}aya(say)sstayay)",
    "dirtree": "(a(say)a(sayay))",
    "dirmeta": "(uuua(ayay))",
}

# The vectors below always exercise offset widths of 1 and 2 bytes (and the
# 2 -> 4 byte transition) for arrays of variable-size elements.  GVariant
# picks the offset width from the *total* serialized array size, not just the
# payload size; for a one-element string array the boundaries are:
#   string length 253 -> 255 bytes total -> 1-byte offsets
#   string length 254 -> 257 bytes total -> 2-byte offsets
#   string length 65532 -> 65535 bytes total -> 2-byte offsets
#   string length 65533 -> 65538 bytes total -> 4-byte offsets
AS_OFFSET_253 = 253
AS_OFFSET_254 = 254
AS_OFFSET_65532 = 65532
AS_OFFSET_65533 = 65533

# Tuple offsets are only needed for variable members.  For ``(ss)`` with an
# empty first member there is one offset, so the 1 -> 2 byte boundary occurs
# at a second-string length of 252/253.
TUPLE_SS_OFFSET_252 = 252
TUPLE_SS_OFFSET_253 = 253

# ``a(say)`` elements are tuples (one tuple offset each), so the array offset
# width flips at a string length of 252/253 as well.
A_SAY_OFFSET_252 = 252
A_SAY_OFFSET_253 = 253


# ---------------------------------------------------------------------------
# Small helpers
# ---------------------------------------------------------------------------


def die(message: str) -> "NoReturn":  # noqa: F821 - typing convenience only
    sys.exit(f"error: {message}")


def variant_bytes(variant: "GLib.Variant") -> bytes:
    """Return the serialized bytes of a variant.

    Keeping the GBytes object alive until after ``get_data()`` avoids
    PyGObject lifetime surprises with temporary objects.
    """
    gbytes = variant.get_data_as_bytes()
    return bytes(gbytes.get_data())


def minimal_offset_width(data_size: int, offsets: int) -> int:
    """Smallest GVariant offset width for an array of variable elements.

    GVariant stores absolute offsets, so the last offset has to fit too.
    ``offsets`` is the number of offset entries stored after the element data
    (one per element for arrays; tuple layouts differ and are described in
    their vector notes instead).
    """
    for width in (1, 2, 4, 8):
        if data_size + offsets * width <= (1 << (8 * width)) - 1:
            return width
    raise AssertionError("array too large for a GVariant offset width")


def as_offset_width(strings: list[str]) -> int | None:
    """Offset width for ``as`` with the given strings (``None`` if empty)."""
    if not strings:
        return None
    payload = sum(len(s.encode("utf-8")) + 1 for s in strings)
    return minimal_offset_width(payload, len(strings))


def variant_to_json(variant: "GLib.Variant") -> Any:
    """Convert a GVariant to a JSON-friendly logical value.

    Byte arrays (``ay``) become lists of integers.  Arrays of tuples become
    JSON arrays, dictionaries become JSON objects, and variants become
    ``{"type": ..., "value": ...}`` objects.
    """
    type_string = variant.get_type_string()

    if type_string == "v":
        inner = variant.get_variant()
        return {"type": inner.get_type_string(), "value": variant_to_json(inner)}

    if type_string == "ay":
        return list(variant_bytes(variant))

    if type_string.startswith("a{"):
        entries: dict[str, Any] = {}
        for i in range(variant.n_children()):
            entry = variant.get_child_value(i)
            key = entry.get_child_value(0).get_string()
            entries[key] = variant_to_json(entry.get_child_value(1))
        return {key: entries[key] for key in sorted(entries)}

    if type_string.startswith("{"):
        return [
            variant_to_json(variant.get_child_value(0)),
            variant_to_json(variant.get_child_value(1)),
        ]

    if type_string.startswith("a") or type_string.startswith("(") or type_string.startswith("r"):
        return [
            variant_to_json(variant.get_child_value(i))
            for i in range(variant.n_children())
        ]

    if type_string in ("y", "n", "q", "i", "u", "x", "t"):
        getters = {
            "y": "get_byte",
            "n": "get_int16",
            "q": "get_uint16",
            "i": "get_int32",
            "u": "get_uint32",
            "x": "get_int64",
            "t": "get_uint64",
        }
        return int(getattr(variant, getters[type_string])())
    if type_string == "h":
        return int(variant.get_handle())
    if type_string == "d":
        return float(variant.get_double())
    if type_string == "b":
        return bool(variant.get_boolean())
    if type_string in ("s", "o", "g"):
        return variant.get_string()

    die(f"cannot convert GVariant type {type_string!r} to a JSON value")


def make_vector(
    name: str,
    category: str,
    type_string: str,
    value: Any,
    *,
    note: str | None = None,
    text_parseable: bool = True,
    offset_width: int | None = None,
) -> dict[str, Any]:
    """Build one vector and verify the GLib text form round-trips."""
    variant = GLib.Variant(type_string, value)
    data = variant_bytes(variant)
    text = variant.print_(True)

    if text_parseable:
        reparsed = GLib.Variant.parse(GLib.VariantType.new(type_string), text, None, None)
        reparsed_data = variant_bytes(reparsed)
        if reparsed_data != data:
            raise AssertionError(
                f"vector {name!r}: GLib text does not round-trip\n"
                f"  text:   {text!r}\n"
                f"  bytes:  {data.hex()}\n"
                f"  parsed: {reparsed_data.hex()}"
            )

    vector: dict[str, Any] = {
        "name": name,
        "category": category,
        "type": type_string,
        "value": variant_to_json(variant),
        "text": text,
        "hex": data.hex(),
        "size": len(data),
        "text_parseable": text_parseable,
    }
    if note:
        vector["note"] = note
    if offset_width is not None:
        vector["offset_width"] = offset_width
    return vector


# ---------------------------------------------------------------------------
# Vector definitions
# ---------------------------------------------------------------------------


def basic_vectors() -> list[dict[str, Any]]:
    vectors: list[dict[str, Any]] = []

    def add(
        name: str,
        type_string: str,
        value: Any,
        note: str | None = None,
        *,
        text_parseable: bool = True,
    ) -> None:
        vectors.append(
            make_vector(
                f"basic/{name}",
                "basic",
                type_string,
                value,
                note=note,
                text_parseable=text_parseable,
            )
        )

    # y: byte
    add("byte-0", "y", 0, "minimum byte")
    add("byte-1", "y", 1)
    add("byte-127", "y", 127)
    add("byte-255", "y", 255, "maximum byte")

    # n: int16
    add("int16-min", "n", -32768)
    add("int16-minus-one", "n", -1)
    add("int16-zero", "n", 0)
    add("int16-one", "n", 1)
    add("int16-max", "n", 32767)

    # q: uint16
    add("uint16-0", "q", 0)
    add("uint16-1", "q", 1)
    add("uint16-255", "q", 255)
    add("uint16-256", "q", 256, "first value not representable in a byte")
    add("uint16-max", "q", 65535)

    # i: int32
    add("int32-min", "i", -(2**31))
    add("int32-minus-one", "i", -1)
    add("int32-zero", "i", 0)
    add("int32-one", "i", 1)
    add("int32-max", "i", 2**31 - 1)

    # u: uint32
    add("uint32-0", "u", 0)
    add("uint32-1", "u", 1)
    add("uint32-65535", "u", 65535)
    add("uint32-65536", "u", 65536)
    add("uint32-max", "u", 2**32 - 1)

    # x: int64
    add("int64-min", "x", -(2**63))
    add("int64-minus-one", "x", -1)
    add("int64-zero", "x", 0)
    add("int64-max", "x", 2**63 - 1)

    # t: uint64
    add("uint64-0", "t", 0)
    add("uint64-1", "t", 1)
    add("uint64-2pow32", "t", 2**32)
    add("uint64-max", "t", 2**64 - 1)

    # h: handle (int32)
    add("handle-minus-one", "h", -1)
    add("handle-zero", "h", 0)
    add("handle-max", "h", 2**31 - 1)

    # d: double
    add("double-negative-max", "d", -1.7976931348623157e308)
    add("double-minus-1.5", "d", -1.5)
    add("double-negative-zero", "d", -0.0, "IEEE 754 negative zero")
    add("double-zero", "d", 0.0)
    add("double-1.5", "d", 1.5)
    add("double-pi", "d", 3.141592653589793)
    add("double-min-normal", "d", 2.2250738585072014e-308)
    add("double-max", "d", 1.7976931348623157e308)
    add(
        "double-min-subnormal",
        "d",
        5e-324,
        "smallest positive subnormal double; GLib's text printer emits "
        "4.9406564584124654e-324, which g_variant_parse rejects as out of range",
        text_parseable=False,
    )

    # b: boolean
    add("boolean-false", "b", False)
    add("boolean-true", "b", True)

    # s: string
    add("string-empty", "s", "", "empty string")
    add("string-ascii", "s", "hello")
    add("string-utf8", "s", "héllo 世界 🚀", "multi-byte UTF-8")
    add("string-controls", "s", "line\nbreak\ttab", "text form must escape control characters")
    add("string-quotes-backslash", "s", "quote' and \\ backslash")
    add("string-300", "s", "x" * 300, "long single string")

    # o: object path
    add("objectpath-root", "o", "/")
    add("objectpath-short", "o", "/org/example/Test")
    add("objectpath-ostree", "o", "/org/ostree/Repo/One")

    # g: signature
    add("signature-empty", "g", "")
    add("signature-s", "g", "s")
    add("signature-dict", "g", "a{sv}")
    add("signature-say", "g", "a(say)")
    add("signature-commit", "g", "(a{sv}aya(say)sstayay)")

    return vectors


def container_vectors() -> list[dict[str, Any]]:
    vectors: list[dict[str, Any]] = []

    def add(
        name: str,
        type_string: str,
        value: Any,
        note: str | None = None,
        *,
        offset_width: int | None = None,
    ) -> None:
        if type_string == "as":
            computed = as_offset_width(value)
            if computed is not None:
                if offset_width is not None and offset_width != computed:
                    raise AssertionError(
                        f"vector {name!r}: declared offset width {offset_width} "
                        f"differs from computed width {computed}"
                    )
                offset_width = computed
        vectors.append(
            make_vector(
                f"container/{name}",
                "container",
                type_string,
                value,
                note=note,
                offset_width=offset_width,
            )
        )

    # ay: array of bytes (fixed-size elements; no offset table)
    add("ay-empty", "ay", b"")
    add("ay-one-zero", "ay", b"\x00", "GLib prints this as b'' (one NUL byte)")
    add("ay-0-1-2", "ay", b"\x00\x01\x02")
    add("ay-deadbeef", "ay", b"\xde\xad\xbe\xef")
    add("ay-all-byte-values", "ay", bytes(range(256)), "every possible byte value")
    add("ay-255-zeros", "ay", b"\x00" * 255, "255 bytes: no offset table for ay")
    add("ay-256-ff", "ay", b"\xff" * 256, "256 bytes: no offset table for ay")

    # as: arrays of strings; offset width boundaries are called out in notes.
    add("as-empty", "as", [], "empty array has no offset table")
    add("as-one", "as", ["a"])
    add("as-two", "as", ["a", "b"])
    add("as-three", "as", ["a", "b", "c"])
    add("as-one-empty", "as", [""])
    add("as-three-empty", "as", ["", "", ""])
    add("as-utf8", "as", ["héllo", "世界", ""])
    add(
        "as-string-252",
        "as",
        ["x" * 252],
        "payload 253 bytes, total 254 bytes: 1-byte offsets",
    )
    add(
        "as-string-253",
        "as",
        ["x" * AS_OFFSET_253],
        "payload 254 bytes, total 255 bytes: 1-byte offsets (maximum)",
    )
    add(
        "as-string-254",
        "as",
        ["x" * AS_OFFSET_254],
        "payload 255 bytes, total 257 bytes: 2-byte offsets (minimum)",
    )
    add(
        "as-string-255",
        "as",
        ["x" * 255],
        "payload 256 bytes, total 258 bytes: 2-byte offsets",
    )
    add(
        "as-string-65532",
        "as",
        ["x" * AS_OFFSET_65532],
        "payload 65533 bytes, total 65535 bytes: 2-byte offsets (maximum)",
    )
    add(
        "as-string-65533",
        "as",
        ["x" * AS_OFFSET_65533],
        "payload 65534 bytes, total 65538 bytes: 4-byte offsets (minimum)",
    )
    add(
        "as-three-long",
        "as",
        ["x" * 100, "y" * 100, "z" * 100],
        "payload 303 bytes across three elements: 2-byte offsets despite "
        "each element fitting in a byte",
    )

    # a(say): array of (string, byte array)
    add("a-say-empty", "a(say)", [])
    add("a-say-one", "a(say)", [("a", b"")])
    add("a-say-two", "a(say)", [("a", b"\x00\x01"), ("héllo", b"")])
    add("a-say-binary", "a(say)", [("name", b"\xde\xad\xbe\xef"), ("", b"")])
    add(
        "a-say-string-252",
        "a(say)",
        [("x" * A_SAY_OFFSET_252, b"")],
        "element is 254 bytes, array total 255 bytes: 1-byte offsets",
        offset_width=1,
    )
    add(
        "a-say-string-253",
        "a(say)",
        [("x" * A_SAY_OFFSET_253, b"")],
        "element is 255 bytes, array total 257 bytes: 2-byte offsets",
        offset_width=2,
    )

    # a{sv}: dictionary of string -> variant
    add("a-sv-empty", "a{sv}", {})
    add("a-sv-one", "a{sv}", {"a": GLib.Variant("i", 1)})
    add(
        "a-sv-two",
        "a{sv}",
        {
            "a": GLib.Variant("s", "x"),
            "b": GLib.Variant("as", ["y", "z"]),
        },
    )
    add(
        "a-sv-ostree-metadata",
        "a{sv}",
        {"ostree.ref-binding": GLib.Variant("as", ["test"])},
        "metadata emitted by ostree commit --branch=test",
    )
    add(
        "a-sv-nested-variant",
        "a{sv}",
        {"n": GLib.Variant("v", GLib.Variant("u", 7))},
    )
    add("a-sv-empty-key", "a{sv}", {"": GLib.Variant("b", True)})

    # Tuples
    add("sv", "(sv)", ("a", GLib.Variant("i", 1)))
    add("sv-utf8", "(sv)", ("héllo", GLib.Variant("as", ["x"])))

    add("ss-empty", "(ss)", ("", ""))
    add("ss-subject-body", "(ss)", ("subject", "body"))
    add("ss-utf8", "(ss)", ("héllo", "世界"))
    add(
        "ss-string-252",
        "(ss)",
        ("", "x" * TUPLE_SS_OFFSET_252),
        "payload 254 bytes plus one tuple offset = 255 bytes: 1-byte offsets",
        offset_width=1,
    )
    add(
        "ss-string-253",
        "(ss)",
        ("", "x" * TUPLE_SS_OFFSET_253),
        "payload 255 bytes; the tuple offset no longer fits in one byte: "
        "2-byte offsets",
        offset_width=2,
    )

    add("sss", "(sss)", ("a", "b", "c"))
    add("sss-empty", "(sss)", ("", "", ""))

    add("ssy-zero", "(ssy)", ("a", "b", 0))
    add("ssy-max", "(ssy)", ("héllo", "世界", 255))

    add("say-empty", "(say)", ("", b""))
    add("say-bytes", "(say)", ("name", b"\x00\x01"))

    # Commit-shaped tuple: a{sv} ay a(say) s s t ay ay
    add(
        "commit-empty",
        "(a{sv}aya(say)sstayay)",
        ({}, b"", [], "", "", 0, b"", b""),
        "all commit fields empty/zero",
    )
    add(
        "commit-filled",
        "(a{sv}aya(say)sstayay)",
        (
            {"ostree.ref-binding": GLib.Variant("as", ["test"])},
            b"",
            [("f", bytes(range(32)))],
            "subj",
            "body",
            1700000000,
            bytes([0x11] * 32),
            bytes([0x22] * 32),
        ),
        "commit-shaped value with a 32-byte related object checksum, "
        "timestamp and two 32-byte checksums",
    )

    # v: variants, including nested variants
    add("v-int32", "v", GLib.Variant("i", 42))
    add("v-string", "v", GLib.Variant("s", "héllo"))
    add("v-ay", "v", GLib.Variant("ay", b"\x00\x01"))
    add("v-tuple", "v", GLib.Variant("(ss)", ("a", "b")))
    add("v-nested", "v", GLib.Variant("v", GLib.Variant("as", ["x"])))
    add(
        "v-nested-dict",
        "v",
        GLib.Variant("a{sv}", {"a": GLib.Variant("i", 1)}),
    )

    return vectors


# ---------------------------------------------------------------------------
# Real OSTree objects
# ---------------------------------------------------------------------------


def run(command: list[str]) -> None:
    try:
        subprocess.run(command, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    except FileNotFoundError:
        die(f"command not found: {command[0]}")
    except subprocess.CalledProcessError as exc:
        stderr = exc.stderr.decode("utf-8", "replace") if exc.stderr else ""
        die(f"command failed ({exc.returncode}): {' '.join(command)}\n{stderr}")


def probe_has_objects(probe_root: Path) -> bool:
    objects = probe_root / "repo" / "objects"
    if not objects.is_dir():
        return False
    return any(
        path.suffix in (".commit", ".dirtree", ".dirmeta")
        for path in objects.glob("*/*")
    )


def create_probe_repository() -> Path:
    """Create a tiny deterministic bare repo and return its temporary root."""
    if not Path(OSTREE).exists():
        die(f"ostree executable not found: {OSTREE}")

    root = Path(tempfile.mkdtemp(prefix="ostree-gvariant-probe-"))
    repo = root / "repo"
    src = root / "src"
    (src / "sub").mkdir(parents=True)
    (src / "a.txt").write_text("a\n", encoding="utf-8")
    (src / "sub" / "b.txt").write_text("b\n", encoding="utf-8")

    run([OSTREE, f"--repo={repo}", "init", "--mode=bare"])
    run(
        [
            OSTREE,
            f"--repo={repo}",
            "commit",
            "--branch=test",
            f"--tree=dir={src}",
            "--timestamp=@0",
            "--no-xattrs",
            "-s",
            "subj",
            "-m",
            "body",
        ]
    )
    return root


def find_source_objects(probe_root: Path) -> list[tuple[str, str, Path]]:
    """Return sorted (kind, checksum, path) entries from the probe repo."""
    objects: list[tuple[str, str, Path]] = []
    objects_root = probe_root / "repo" / "objects"
    for path in sorted(objects_root.glob("*/*")):
        kind = path.suffix[1:]
        if kind not in OBJECT_TYPES:
            continue
        if not path.is_file():
            continue
        # OSTree shards objects as objects/<first-2-hex>/<remaining-hex>.<kind>;
        # the object checksum is the directory name plus the file stem.
        checksum = path.parent.name + path.stem
        objects.append((kind, checksum, path))
    objects.sort(key=lambda item: (item[0], item[1]))
    return objects


def decode_xattrs(value: "GLib.Variant") -> list[dict[str, Any]]:
    result: list[dict[str, Any]] = []
    for i in range(value.n_children()):
        entry = value.get_child_value(i)
        name = variant_bytes(entry.get_child_value(0))
        xattr_value = variant_bytes(entry.get_child_value(1))
        item: dict[str, Any] = {
            "name_hex": name.hex(),
            "value_hex": xattr_value.hex(),
        }
        try:
            item["name"] = name.decode("utf-8")
        except UnicodeDecodeError:
            pass
        result.append(item)
    return result


def decode_commit(value: "GLib.Variant") -> dict[str, Any]:
    parent = variant_bytes(value.get_child_value(1))
    related_value = value.get_child_value(2)
    related = []
    for i in range(related_value.n_children()):
        entry = related_value.get_child_value(i)
        related.append(
            {
                "name": entry.get_child_value(0).get_string(),
                "checksum": variant_bytes(entry.get_child_value(1)).hex(),
            }
        )

    # The timestamp is stored as a u64 in OSTree's big-endian convention.
    raw_timestamp = int(value.get_child_value(5).get_uint64())
    timestamp = int.from_bytes(raw_timestamp.to_bytes(8, "little"), "big")

    return {
        "metadata": variant_to_json(value.get_child_value(0)),
        "parent": parent.hex(),
        "related": related,
        "subject": value.get_child_value(3).get_string(),
        "body": value.get_child_value(4).get_string(),
        "timestamp": timestamp,
        "root_tree": variant_bytes(value.get_child_value(6)).hex(),
        "root_metadata": variant_bytes(value.get_child_value(7)).hex(),
    }


def decode_dirtree(value: "GLib.Variant") -> dict[str, Any]:
    files = []
    files_value = value.get_child_value(0)
    for i in range(files_value.n_children()):
        entry = files_value.get_child_value(i)
        files.append(
            {
                "name": entry.get_child_value(0).get_string(),
                "checksum": variant_bytes(entry.get_child_value(1)).hex(),
            }
        )

    directories = []
    dirs_value = value.get_child_value(1)
    for i in range(dirs_value.n_children()):
        entry = dirs_value.get_child_value(i)
        directories.append(
            {
                "name": entry.get_child_value(0).get_string(),
                "tree_checksum": variant_bytes(entry.get_child_value(1)).hex(),
                "meta_checksum": variant_bytes(entry.get_child_value(2)).hex(),
            }
        )

    return {"files": files, "directories": directories}


def decode_dirmeta(value: "GLib.Variant", data: bytes) -> dict[str, Any]:
    # (uuua(ayay)): the three u32 values are the first fixed-size members.
    return {
        "uid": int.from_bytes(data[0:4], "big"),
        "gid": int.from_bytes(data[4:8], "big"),
        "mode": int.from_bytes(data[8:12], "big"),
        "xattrs": decode_xattrs(value.get_child_value(3)),
    }


def collect_objects(probe_root: Path) -> list[dict[str, Any]]:
    source_objects = find_source_objects(probe_root)
    if not source_objects:
        die(f"no .commit/.dirtree/.dirmeta objects found under {probe_root}")

    if OBJECTS_DIR.exists():
        shutil.rmtree(OBJECTS_DIR)
    OBJECTS_DIR.mkdir(parents=True)

    objects: list[dict[str, Any]] = []
    for kind, checksum, source in source_objects:
        data = source.read_bytes()
        target = OBJECTS_DIR / f"{checksum}.{kind}"
        target.write_bytes(data)

        type_string = OBJECT_TYPES[kind]
        variant = GLib.Variant.new_from_bytes(
            GLib.VariantType.new(type_string), GLib.Bytes.new(data), False
        )
        if variant.get_size() != len(data):
            die(
                f"object {target.name}: serialized size {variant.get_size()} "
                f"does not match file size {len(data)}"
            )
        if not variant.is_normal_form():
            die(f"object {target.name}: not a normal-form GVariant")

        algorithm = {64: "sha256", 40: "sha1"}.get(len(checksum), "unknown")
        digest = (
            hashlib.sha256(data).hexdigest()
            if algorithm == "sha256"
            else hashlib.sha1(data).hexdigest()
            if algorithm == "sha1"
            else ""
        )

        if kind == "commit":
            decoded = decode_commit(variant)
        elif kind == "dirtree":
            decoded = decode_dirtree(variant)
        else:
            decoded = decode_dirmeta(variant, data)

        object_entry: dict[str, Any] = {
            "name": f"{kind}/{checksum}",
            "kind": kind,
            "type": type_string,
            "checksum": checksum,
            "checksum_algorithm": algorithm,
            "checksum_verified": digest == checksum,
            "path": f"ostree-objects/{target.name}",
            "size": len(data),
            "hex": data.hex(),
            "sha256": hashlib.sha256(data).hexdigest(),
            "scalar_endianness": "big",
            "decoded": decoded,
        }
        if not object_entry["checksum_verified"]:
            print(
                f"warning: {target.name}: file content does not hash to its "
                f"OSTree checksum with {algorithm}",
                file=sys.stderr,
            )
        objects.append(object_entry)

    add_compatibility_aliases(objects)
    return objects


def add_compatibility_aliases(objects: list[dict[str, Any]]) -> None:
    """Create stable, human-readable copies used by the Go tests.

    Canonical files are named ``<checksum>.<kind>``.  A few tests use the
    shorter names ``commit.bin``, ``dirmeta.bin`` and ``dirtree-root.bin``
    (and ``dirtree-sub.bin`` for the first child directory).  The JSON keeps
    pointing at the canonical checksum-named files; the aliases are recorded
    in each object's ``aliases`` list.
    """
    by_kind: dict[str, list[dict[str, Any]]] = {}
    for obj in objects:
        by_kind.setdefault(obj["kind"], []).append(obj)

    aliases: dict[str, list[str]] = {}
    commit = by_kind.get("commit", [None])[0]
    if commit is not None:
        aliases[commit["path"]] = ["commit.bin"]
        root_tree = commit["decoded"]["root_tree"]
        root_meta = commit["decoded"]["root_metadata"]
        for obj in by_kind.get("dirtree", []):
            if obj["checksum"] == root_tree:
                aliases.setdefault(obj["path"], []).append("dirtree-root.bin")
                directories = obj["decoded"]["directories"]
                if directories:
                    sub_tree = directories[0]["tree_checksum"]
                    for candidate in by_kind.get("dirtree", []):
                        if candidate["checksum"] == sub_tree:
                            aliases.setdefault(candidate["path"], []).append(
                                "dirtree-sub.bin"
                            )
        for obj in by_kind.get("dirmeta", []):
            if obj["checksum"] == root_meta:
                aliases.setdefault(obj["path"], []).append("dirmeta.bin")

    for obj in objects:
        names = aliases.get(obj["path"])
        if not names:
            continue
        source = OBJECTS_DIR / Path(obj["path"]).name
        for name in names:
            target = OBJECTS_DIR / name
            if target.exists():
                die(f"compatibility alias already exists: {target}")
            shutil.copyfile(source, target)
        obj["aliases"] = names


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def main() -> None:
    if sys.byteorder != "little":
        die(
            "this generator emits little-endian GVariant serialization; "
            f"current host is {sys.byteorder}-endian"
        )

    vectors = basic_vectors() + container_vectors()

    names = [vector["name"] for vector in vectors]
    if len(names) != len(set(names)):
        duplicates = sorted({name for name in names if names.count(name) > 1})
        die(f"duplicate vector names: {duplicates}")

    probe_root = PROBE_ROOT
    temporary_probe: Path | None = None
    if not probe_has_objects(probe_root):
        temporary_probe = create_probe_repository()
        probe_root = temporary_probe

    try:
        objects = collect_objects(probe_root)
    finally:
        if temporary_probe is not None:
            shutil.rmtree(temporary_probe, ignore_errors=True)

    document = {
        "schema": "ostree-go/internal/gvariant/testdata/vectors.json",
        "version": 1,
        "generator": "go/hack/gen-gvariant-vectors.py",
        "description": (
            "Authoritative GVariant serialization vectors generated with "
            "PyGObject/GLib. 'value' is a JSON-native logical value, 'text' is "
            "the canonical GLib GVariant text representation, and 'hex' is the "
            "serialized byte sequence in host (little-endian) order. "
            "'text_parseable' is false only when GLib's own text printer "
            "emits a representation its parser rejects. Real OSTree objects "
            "under ostree-objects/ are raw object files: GVariant framing is "
            "host-endian, but OSTree stores fixed-width scalar payloads in "
            "big-endian order."
        ),
        "glib_version": f"{GLib.MAJOR_VERSION}.{GLib.MINOR_VERSION}.{GLib.MICRO_VERSION}",
        "host_endianness": sys.byteorder,
        "vector_count": len(vectors),
        "object_count": len(objects),
        "vectors": vectors,
        "objects": objects,
    }

    TESTDATA_DIR.mkdir(parents=True, exist_ok=True)
    temporary_json = VECTORS_PATH.with_name(VECTORS_PATH.name + ".tmp")
    with temporary_json.open("w", encoding="utf-8") as handle:
        json.dump(document, handle, indent=2, ensure_ascii=False)
        handle.write("\n")
    temporary_json.replace(VECTORS_PATH)

    categories: dict[str, int] = {}
    for vector in vectors:
        categories[vector["category"]] = categories.get(vector["category"], 0) + 1

    print(f"wrote {len(vectors)} vectors to {VECTORS_PATH}")
    print(f"copied {len(objects)} OSTree objects to {OBJECTS_DIR}")
    print("vector categories: " + ", ".join(f"{k}={v}" for k, v in sorted(categories.items())))
    print(
        "offset boundaries: "
        f"as/{AS_OFFSET_253},{AS_OFFSET_254} (1->2 bytes), "
        f"as/{AS_OFFSET_65532},{AS_OFFSET_65533} (2->4 bytes), "
        f"(ss)/{TUPLE_SS_OFFSET_252},{TUPLE_SS_OFFSET_253} (1->2 bytes), "
        f"a(say)/{A_SAY_OFFSET_252},{A_SAY_OFFSET_253} (1->2 bytes)"
    )
    print("objects: " + ", ".join(f"{obj['kind']}={obj['checksum'][:12]}" for obj in objects))


if __name__ == "__main__":
    main()
