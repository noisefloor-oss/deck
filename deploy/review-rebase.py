#!/usr/bin/env python3
"""Conservative evidence for an independent mechanical-rebase assessment."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess

ALGORITHM = "changed-file-objects-v1"


def git(*args):
    if args and args[0] == "diff":
        args = ("diff", "--no-relative", "--ignore-submodules=none", *args[1:])
    return subprocess.check_output(["git", "--no-replace-objects", *args])


def commit(value):
    if not re.fullmatch("[0-9a-f]{40}", value):
        raise ValueError("every revision must be an immutable full SHA")
    if git("rev-parse", value + "^{commit}").decode().strip() != value:
        raise ValueError("revision is not the named commit")
    return value


def merge_base(base, head):
    bases = git("merge-base", "--all", base, head).decode().splitlines()
    if len(bases) != 1:
        raise ValueError("ambiguous merge base requires full review")
    return bases[0]


def changed_objects(base, head):
    # Whole-file blob identity is stricter than an edit fingerprint. Context
    # changes intentionally require full review instead of guessing placement.
    fields = git("diff", "--raw", "-z", "--no-abbrev", "--find-renames", "--no-ext-diff", "--no-textconv", base, head).split(b"\0")
    entries = []
    i = 0
    while i < len(fields) - 1:
        header = fields[i].split()
        if len(header) != 5 or not header[0].startswith(b":"):
            raise ValueError("unsupported diff record requires full review")
        old_mode, new_mode, old_blob, new_blob, kind = header
        old_mode = old_mode[1:]
        if kind not in (b"M", b"A", b"D"):
            raise ValueError("renames, copies and type changes require full review")
        if old_mode not in (b"000000", b"100644", b"100755") or new_mode not in (b"000000", b"100644", b"100755"):
            raise ValueError("symlink or submodule changes require full review")
        path = fields[i + 1]
        # Hex path encoding retains arbitrary path bytes and avoids delimiter ambiguity.
        entries.append(dict(path_hex=path.hex(), kind=kind.decode(), old_mode=old_mode.decode(),
                            new_mode=new_mode.decode(), old_blob=old_blob.decode(), new_blob=new_blob.decode()))
        i += 2
    return entries


def fingerprint(entries):
    return hashlib.sha256(json.dumps(dict(algorithm=ALGORITHM, entries=entries), sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def compare(old_base, old_head, new_base, new_head):
    old_base, old_head, new_base, new_head = map(commit, (old_base, old_head, new_base, new_head))
    old_merge = merge_base(old_base, old_head)
    new_merge = merge_base(new_base, new_head)
    result = dict(algorithm=ALGORITHM, old_base=old_base, old_head=old_head, new_base=new_base, new_head=new_head,
                  old_merge_base=old_merge, new_merge_base=new_merge, eligible_for_bounded_review=False,
                  requirements=["Verify the original review actually read old_head", "Independently assess the entire base delta and inherited stack changes", "Require fresh exact-head formal approval and current CI", "Any interaction or uncertainty requires full review"])
    try:
        before, after = changed_objects(old_merge, old_head), changed_objects(new_merge, new_head)
        result.update(old_fingerprint=fingerprint(before), new_fingerprint=fingerprint(after))
        result["eligible_for_bounded_review"] = bool(before) and before == after
        result["reason"] = "identical changed-file objects; independent base assessment still required" if result["eligible_for_bounded_review"] else "changed file contents, paths or modes require full review"
    except ValueError as error:
        result["reason"] = str(error)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("old-base", "old-head", "new-base", "new-head", "output-dir"):
        parser.add_argument("--" + name, required=True)
    args = parser.parse_args()
    result = compare(args.old_base, args.old_head, args.new_base, args.new_head)
    destination = Path(args.output_dir)
    destination.mkdir(mode=0o700, parents=False, exist_ok=False)
    (destination / "assessment.json").write_text(json.dumps(result, indent=2) + "\n")
    for name, base, head in (("old.patch", result["old_merge_base"], args.old_head),
                             ("new.patch", result["new_merge_base"], args.new_head),
                             ("base.patch", result["old_merge_base"], result["new_merge_base"])):
        (destination / name).write_bytes(git("diff", "--binary", "--full-index", "--no-ext-diff", "--no-textconv", base, head))
    print(json.dumps(result, indent=2))
    return 0 if result["eligible_for_bounded_review"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
