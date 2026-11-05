#!/usr/bin/env python3
"""Host-side half of Deck's encrypted-state gate.

The container cannot classify its own backing on every host: a btrfs bind
mount shows an anonymous device with no sysfs entry, and the host's device
list is not visible from inside. This script runs on the host, where the
whole picture is, in two steps that the compose wrapper calls around
``docker compose up``:

``qualify`` runs before the container exists. It resolves every host path
behind a store to its backing and refuses unless the profile's rules hold:
dm-crypt for the own-host profile, a gocryptfs mount owned by the partner uid
without allow_other or allow_root and with the kernel's allow_sys_admin_access
off for the shared-host profile; personal swap by default or verified dm-crypt
swap in opt-in paranoid mode;
no hibernation resume device on either storage profile.

``report`` runs after the container is created and while its workloads are
still gated. It reads the container's own mount table through
``/proc/<init pid>/mountinfo``, correlates each store's container mount back
to a host source by device and root, refuses when any store maps to a source
it did not qualify, and only then writes the report the container
cross-checks: kernel boot id, container id, profile, and per store the
container-visible mount id, device, root and backing. The report lands in a
host-owned tmpfs the wrapper bind-mounts read-only at
``/run/noisedeck/sealed-state``.

Everything the script decides comes from the kernel: mountinfo, sysfs, and
procfs. A marker file, an environment variable, or a previous run's report
is never an input.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
from dataclasses import dataclass, field
from pathlib import Path

PROFILE_SHARED = "shared-host"
PROFILE_OWN = "own-host"

BACKING_DM_CRYPT = "dm-crypt"
BACKING_GOCRYPTFS = "gocryptfs"
BACKING_TMPFS = "tmpfs"
BACKING_PLAINTEXT = "plaintext"
BACKING_UNRESOLVED = "unresolved"

# Container path per store, mirrored in internal/sealedstate.DefaultStores.
CONTAINER_PATHS = {
    "data": "/data",
    "home": "/home/agent",
    "projects": "/workspace",
    "tmp": "/tmp",
    "palace": "/home/agent/.mempalace",
    "gh-config": "/home/agent/.config/gh",
}
REQUIRED_STORES = ("data", "home", "projects", "tmp")

# Which backings each profile treats as sealed. dm-crypt does not count on
# the shared host, because another user unlocked it and reads its
# plaintext; tmpfs does not count there because root reads a container
# tmpfs through /proc/<pid>/root. On the own host tmpfs is accepted for the
# tmp store only.
ACCEPTED = {
    PROFILE_SHARED: {BACKING_GOCRYPTFS},
    PROFILE_OWN: {BACKING_DM_CRYPT, BACKING_TMPFS},
}


class PreflightError(Exception):
    """A refusal that names the path and the reason."""


@dataclass
class Mount:
    id: int
    parent: int
    device: str
    root: str
    mount_point: str
    fstype: str
    source: str
    super_opts: str


def _unescape(value: str) -> str:
    out = []
    i = 0
    while i < len(value):
        if value[i] == "\\" and i + 3 < len(value) + 0 and value[i + 1 : i + 4].isdigit():
            out.append(chr(int(value[i + 1 : i + 4], 8)))
            i += 4
            continue
        out.append(value[i])
        i += 1
    return "".join(out)


def parse_mountinfo(text: str) -> list[Mount]:
    mounts: list[Mount] = []
    for line in text.splitlines():
        line = line.strip()
        if not line:
            continue
        fields = line.split()
        try:
            sep = fields.index("-")
        except ValueError as exc:
            raise PreflightError(f"malformed mountinfo line: {line!r}") from exc
        if sep < 6 or sep + 2 >= len(fields):
            raise PreflightError(f"malformed mountinfo line: {line!r}")
        mounts.append(
            Mount(
                id=int(fields[0]),
                parent=int(fields[1]),
                device=fields[2],
                root=_unescape(fields[3]),
                mount_point=_unescape(fields[4]),
                fstype=fields[sep + 1],
                source=_unescape(fields[sep + 2]),
                super_opts=fields[sep + 3] if sep + 3 < len(fields) else "",
            )
        )
    return mounts


def mount_for(mounts: list[Mount], path: str) -> Mount | None:
    """The mount whose mount point is the longest prefix of path; later
    lines win at equal length, as the kernel stacks mounts."""
    path = os.path.normpath(path)
    best: Mount | None = None
    best_len = -1
    for m in mounts:
        mp = os.path.normpath(m.mount_point)
        if mp != "/" and path != mp and not path.startswith(mp + "/"):
            continue
        if len(mp) >= best_len:
            best, best_len = m, len(mp)
    return best


@dataclass
class Host:
    """The kernel facts the host preflight reads. Tests point every path at
    a fixture tree; the binary uses the live kernel."""

    proc: Path = Path("/proc")
    sys: Path = Path("/sys")

    def mounts(self) -> list[Mount]:
        return parse_mountinfo((self.proc / "self" / "mountinfo").read_text())

    def dm_uuid(self, device: str) -> str:
        try:
            return (self.sys / "dev" / "block" / device / "dm" / "uuid").read_text().strip()
        except OSError:
            return ""

    def btrfs_devices(self, fs_uuid: str) -> list[str]:
        """The block devices behind a btrfs filesystem, as major:minor."""
        base = self.sys / "fs" / "btrfs" / fs_uuid / "devices"
        out = []
        try:
            names = sorted(p.name for p in base.iterdir())
        except OSError:
            return out
        for name in names:
            try:
                out.append((self.sys / "class" / "block" / name / "dev").read_text().strip())
            except OSError:
                continue
        return out

    def source_device(self, source: str) -> str:
        """The major:minor of a block device node such as /dev/mapper/x."""
        try:
            st = os.stat(source)
        except OSError:
            return ""
        return f"{os.major(st.st_rdev)}:{os.minor(st.st_rdev)}"

    def btrfs_uuid_for_source(self, source: str) -> str:
        """Find the btrfs filesystem whose device list contains the mount's
        source device. sysfs names devices by kernel name (dm-0, nvme0n1p4)
        while mountinfo names the node you mounted, so the match is
        by device number, never by name."""
        want = self.source_device(source)
        if not want:
            return ""
        base = self.sys / "fs" / "btrfs"
        try:
            for fs in sorted(base.iterdir()):
                devices = fs / "devices"
                if not devices.is_dir():
                    continue
                for dev in devices.iterdir():
                    try:
                        num = (self.sys / "class" / "block" / dev.name / "dev").read_text().strip()
                    except OSError:
                        continue
                    if num == want:
                        return fs.name
        except OSError:
            pass
        return ""

    def fuse_allow_sys_admin_access(self) -> str:
        try:
            return (self.sys / "module" / "fuse" / "parameters" / "allow_sys_admin_access").read_text().strip()
        except OSError:
            return ""

    def swaps(self) -> list[str]:
        return list(self.swap_table())

    def swap_table(self) -> dict[str, str]:
        try:
            lines = (self.proc / "swaps").read_text().strip().splitlines()
        except OSError as exc:
            raise PreflightError("cannot read kernel swap table") from exc
        if not lines or len(lines[0].split()) != 5 or lines[0].split()[:2] != ["Filename", "Type"]:
            raise PreflightError("invalid kernel swap table")
        swaps = {}
        for line in lines[1:]:
            fields = line.split()
            if len(fields) != 5 or not all(value.isdigit() for value in fields[2:4]):
                raise PreflightError("invalid kernel swap entry")
            path = _unescape(fields[0])
            if path in swaps:
                raise PreflightError("duplicate kernel swap entry")
            swaps[path] = fields[1]
        return swaps

    def swap_device(self, path: str) -> str:
        # Only a direct block mapping is independently visible inside the
        # container too. A host swap file cannot be resolved in that namespace.
        if self.swap_table().get(path) != "partition" or os.path.normpath(path) != path:
            return ""
        base = os.path.basename(path)
        candidates = []
        if os.path.dirname(path) == "/dev":
            candidates = [self.sys / "class/block" / base]
        elif os.path.dirname(path) == "/dev/mapper":
            for entry in (self.sys / "class/block").glob("dm-*"):
                try:
                    if (entry / "dm/name").read_text().strip() == base:
                        candidates.append(entry)
                except OSError:
                    continue
        for entry in candidates:
            try:
                return (entry / "dev").read_text().strip()
            except OSError:
                continue
        return ""

    def resume_device(self) -> str:
        try:
            return (self.sys / "power" / "resume").read_text().strip()
        except OSError:
            return ""

    def boot_id(self) -> str:
        return (self.proc / "sys" / "kernel" / "random" / "boot_id").read_text().strip()

    def container_mounts(self, pid: int) -> list[Mount]:
        return parse_mountinfo((self.proc / str(pid) / "mountinfo").read_text())

    def device_for_path(self, path: str) -> str:
        st = os.stat(path)
        return f"{os.major(st.st_dev)}:{os.minor(st.st_dev)}"


@dataclass
class Classification:
    backing: str
    reason: str = ""
    mount: Mount | None = None
    # The host path's location inside its filesystem: mount root joined with
    # the path's offset from the mount point. This is what a container bind
    # mount of the path reports as its root.
    root: str = ""


def _fuse_opts(mount: Mount) -> dict[str, str]:
    opts: dict[str, str] = {}
    for item in mount.super_opts.split(","):
        if "=" in item:
            k, v = item.split("=", 1)
            opts[k] = v
        elif item:
            opts[item] = ""
    return opts


def _dm_crypt(profile: str, m: Mount, root: str) -> Classification:
    if BACKING_DM_CRYPT in ACCEPTED[profile]:
        return Classification(BACKING_DM_CRYPT, "", m, root)
    return Classification(
        BACKING_PLAINTEXT,
        f"dm-crypt is not an accepted backing on the {profile} profile: whoever runs the host unlocked it",
        m,
        root,
    )


def classify_host_path(
    host: Host, mounts: list[Mount], path: str, profile: str, partner_uid: int | None, store_name: str = ""
) -> Classification:
    m = mount_for(mounts, path)
    if m is None:
        return Classification(BACKING_UNRESOLVED, "no mount holds the path")
    rel = os.path.relpath(os.path.normpath(path), os.path.normpath(m.mount_point))
    root = os.path.normpath(m.root if rel == "." else os.path.join(m.root, rel))

    if m.fstype == "fuse.gocryptfs":
        opts = _fuse_opts(m)
        if "allow_other" in opts:
            return Classification(BACKING_PLAINTEXT, "gocryptfs mounted with allow_other; host root reads it", m, root)
        if "allow_root" in opts:
            return Classification(BACKING_PLAINTEXT, "gocryptfs mounted with allow_root; host root reads it", m, root)
        owner = opts.get("user_id", "")
        if owner == "0":
            return Classification(BACKING_PLAINTEXT, "gocryptfs mounted by root; host root reads it", m, root)
        if partner_uid is not None and owner != str(partner_uid):
            return Classification(BACKING_PLAINTEXT, f"gocryptfs mounted by uid {owner or '?'}, not the partner uid {partner_uid}", m, root)
        if host.fuse_allow_sys_admin_access() != "N":
            return Classification(BACKING_PLAINTEXT, "kernel fuse.allow_sys_admin_access is on; host root reads every FUSE mount", m, root)
        if BACKING_GOCRYPTFS not in ACCEPTED[profile]:
            return Classification(BACKING_PLAINTEXT, f"gocryptfs is not an accepted backing on the {profile} profile", m, root)
        return Classification(BACKING_GOCRYPTFS, "", m, root)

    if m.fstype == "tmpfs":
        if BACKING_TMPFS in ACCEPTED[profile] and store_name == "tmp":
            return Classification(BACKING_TMPFS, "", m, root)
        if profile == PROFILE_SHARED:
            return Classification(BACKING_PLAINTEXT, "tmpfs is readable by host root through /proc/<pid>/root on the shared-host profile", m, root)
        return Classification(BACKING_PLAINTEXT, "tmpfs is accepted only for the tmp store", m, root)

    if m.fstype == "btrfs":
        fs_uuid = host.btrfs_uuid_for_source(m.source)
        devices = host.btrfs_devices(fs_uuid) if fs_uuid else []
        if not devices:
            return Classification(BACKING_UNRESOLVED, f"btrfs source {m.source} has no readable device list under /sys/fs/btrfs", m, root)
        for dev in devices:
            uuid = host.dm_uuid(dev)
            if not uuid.startswith("CRYPT-"):
                return Classification(BACKING_PLAINTEXT, f"btrfs device {dev} is not dm-crypt", m, root)
        return _dm_crypt(profile, m, root)

    uuid = host.dm_uuid(m.device)
    if uuid.startswith("CRYPT-"):
        return _dm_crypt(profile, m, root)
    if uuid:
        return Classification(BACKING_PLAINTEXT, f"device-mapper target {m.device} is not dm-crypt", m, root)
    if m.fstype in ("overlay", "fuse", "nfs", "nfs4", "cifs", "9p", "virtiofs"):
        return Classification(BACKING_UNRESOLVED, f"{m.fstype} has no block backing the preflight can classify", m, root)
    return Classification(BACKING_PLAINTEXT, f"{m.fstype} on {m.source} is not encrypted", m, root)


def _sealed(c: Classification) -> bool:
    return c.backing in (BACKING_DM_CRYPT, BACKING_GOCRYPTFS, BACKING_TMPFS)


@dataclass
class Qualification:
    profile: str
    swap_policy: str = "personal"
    stores: dict[str, Classification] = field(default_factory=dict)
    host_paths: dict[str, str] = field(default_factory=dict)


def nested_mounts(mounts: list[Mount], path: str) -> list[Mount]:
    """Every mount whose mount point lies strictly below path."""
    path = os.path.normpath(path)
    out = [m for m in mounts if os.path.normpath(m.mount_point) != path and os.path.normpath(m.mount_point).startswith(path + "/")]
    return sorted(out, key=lambda m: m.mount_point)


def qualify(host: Host, profile: str, stores: dict[str, str], partner_uid: int | None, swap_policy: str = "personal") -> Qualification:
    """Refuse unless every host path behind a store, and swap and
    hibernation, satisfy the profile."""
    if profile not in (PROFILE_SHARED, PROFILE_OWN):
        raise PreflightError(f"unknown profile {profile!r}")
    for name in REQUIRED_STORES:
        if name not in stores:
            raise PreflightError(f"store {name} ({CONTAINER_PATHS[name]}) has no host path; pass --store {name}=<host path>")
    if profile == PROFILE_SHARED and partner_uid is None:
        raise PreflightError("the shared-host profile needs --partner-uid")
    mounts = host.mounts()
    q = Qualification(profile=profile, swap_policy=swap_policy, host_paths=dict(stores))
    failures = []
    for name, path in stores.items():
        if not os.path.isdir(path):
            failures.append(f"{name} ({path}): not a directory on the host")
            continue
        c = classify_host_path(host, mounts, path, profile, partner_uid, name)
        q.stores[name] = c
        if not _sealed(c):
            failures.append(f"{name} ({path}): {c.backing}, {c.reason}")
        # A mount nested below the store's host path is a store of its own:
        # a plaintext bind inside a sealed directory would otherwise hide.
        for nested in nested_mounts(mounts, path):
            nested_name = f"{name}:{os.path.relpath(nested.mount_point, path)}"
            nc = classify_host_path(host, mounts, nested.mount_point, profile, partner_uid, name)
            q.stores[nested_name] = nc
            q.host_paths[nested_name] = nested.mount_point
            if not _sealed(nc):
                failures.append(f"{nested_name} ({nested.mount_point}): {nc.backing}, {nc.reason}")
    if swap_policy not in ("personal", "paranoid"):
        failures.append(f"unknown swap policy {swap_policy!r}")
    swaps = host.swaps()
    if swap_policy == "paranoid":
        if not swaps:
            failures.append("paranoid swap policy requires active encrypted swap")
        for sw in swaps:
            dev = host.swap_device(sw)
            if not dev or not host.dm_uuid(dev).startswith("CRYPT-"):
                failures.append(f"swap {sw} is not verified dm-crypt")
    resume = host.resume_device()
    if resume and resume != "0:0":
        failures.append(f"hibernation resume device is set ({resume}); disable hibernation")
    if failures:
        raise PreflightError("; ".join(sorted(failures)))
    return q


def container_init_pid(engine: str, container: str) -> int:
    """The container's init pid through the engine that launched it; both
    engines answer the same inspect template."""
    import subprocess

    out = subprocess.run(
        [engine, "inspect", "-f", "{{.State.Pid}}", container],
        check=True,
        capture_output=True,
        text=True,
    ).stdout.strip()
    pid = int(out)
    if pid <= 0:
        raise PreflightError(f"container {container} is not running")
    return pid


def container_hostname(engine: str, container: str) -> str:
    """The configured stable hostname read by the in-container sealed check."""
    import subprocess

    return subprocess.run(
        [engine, "inspect", "-f", "{{.Config.Hostname}}", container],
        check=True,
        capture_output=True,
        text=True,
    ).stdout.strip()


def build_report(host: Host, q: Qualification, pid: int, container_id: str) -> dict:
    """Correlate the container's own mounts to the qualified host sources."""
    cmounts = host.container_mounts(pid)
    report_stores = {}
    failures = []
    # Every qualified host classification, keyed by (device, root), is what a
    # container mount must correlate to. A container mount that maps to no
    # qualified source, whether a store or something bound inside one, is a
    # refusal: the host never looked at it.
    qualified = {(c.mount.device, c.root): (name, c) for name, c in q.stores.items() if c.mount is not None}

    def correlate(cpath: str, cm: Mount, label: str) -> None:
        key = (cm.device, os.path.normpath(cm.root))
        if key not in qualified:
            failures.append(
                f"{label} ({cpath}): container mount is {cm.device} root {cm.root}, which no qualified host source matches"
            )
            return
        _, hc = qualified[key]
        report_stores[label] = {
            "path": cpath,
            "mount_id": cm.id,
            "device": cm.device,
            "root": cm.root,
            "backing": hc.backing,
        }

    for name in REQUIRED_STORES:
        cpath = CONTAINER_PATHS[name]
        cm = mount_for(cmounts, cpath)
        if cm is None:
            failures.append(f"{name} ({cpath}): not mounted in the container")
            continue
        if os.path.normpath(cm.mount_point) != os.path.normpath(cpath):
            failures.append(f"{name} ({cpath}): not its own mount in the container (under {cm.mount_point})")
            continue
        correlate(cpath, cm, name)
        for nested in nested_mounts(cmounts, cpath):
            correlate(nested.mount_point, nested, f"{name}:{nested.mount_point}")
    if failures:
        raise PreflightError("; ".join(sorted(failures)))
    return {
        "boot_id": host.boot_id(),
        "container_id": container_id,
        "profile": q.profile,
        "swap_policy": q.swap_policy,
        "stores": report_stores,
    }


def write_report(report: dict, report_dir: Path) -> Path:
    report_dir.mkdir(parents=True, exist_ok=True)
    tmp = report_dir / ".report.json.tmp"
    final = report_dir / "report.json"
    with open(tmp, "w", encoding="utf-8") as fh:
        json.dump(report, fh, indent=2, sort_keys=True)
        fh.write("\n")
        fh.flush()
        os.fsync(fh.fileno())
    os.chmod(tmp, 0o444)
    os.replace(tmp, final)
    return final


def parse_env_file(path: str) -> dict[str, str]:
    """Read a Compose dotenv file the way Compose does, or refuse.

    KEY=VALUE per line; comments and blank lines skipped; an `export` prefix
    allowed. A single-quoted value is literal, no escapes, and nothing may
    follow its closing quote but a comment. A double-quoted value knows the
    escapes \\n \\t \\\\ \\" and \\$. Unquoted and double-quoted values are
    interpolated as Compose interpolates them: $VAR, ${VAR}, ${VAR:-default},
    ${VAR-default}, ${VAR:?error}, ${VAR?error} and $$ for a literal dollar,
    resolved against earlier keys in the file and then the process
    environment. Anything else, an unterminated quote, a stray character
    after a quoted value, a $( or a lone ${, is refused by name rather than
    read as a different value. The wrapper never sources this file, so a
    value with a space, an apostrophe or a backslash is a value, not code.
    """
    values: dict[str, str] = {}
    with open(path, encoding="utf-8") as fh:
        for lineno, raw in enumerate(fh, 1):
            line = raw.strip()
            if not line or line.startswith("#"):
                continue
            if line.startswith("export "):
                line = line[len("export "):].lstrip()
            if "=" not in line:
                raise PreflightError(f"{path}:{lineno}: malformed line {raw.rstrip()!r}")
            key, value = line.split("=", 1)
            key = key.strip()
            if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", key):
                raise PreflightError(f"{path}:{lineno}: malformed key {key!r}")
            value = value.strip()
            where = f"{path}:{lineno}"
            if value.startswith("'"):
                end = value.find("'", 1)
                if end < 0:
                    raise PreflightError(f"{where}: unterminated single quote")
                _only_comment_after(value[end + 1 :], where)
                values[key] = value[1:end]
                continue
            if value.startswith('"'):
                inner, end = _double_quoted(value, where)
                _only_comment_after(value[end + 1 :], where)
                values[key] = _interpolate(inner, values, where)
                continue
            value = value.split(" #", 1)[0].rstrip()
            values[key] = _interpolate(value, values, where)
    return values


def _only_comment_after(rest: str, where: str) -> None:
    rest = rest.strip()
    if rest and not rest.startswith("#"):
        raise PreflightError(f"{where}: unsupported text after the closing quote: {rest!r}")


def _double_quoted(value: str, where: str) -> tuple[str, int]:
    out = []
    i = 1
    escapes = {"n": "\n", "t": "\t", "\\": "\\", '"': '"', "$": "$"}
    while i < len(value):
        c = value[i]
        if c == "\\":
            if i + 1 >= len(value) or value[i + 1] not in escapes:
                raise PreflightError(f"{where}: unsupported escape in double quotes: {value[i:i + 2]!r}")
            if value[i + 1] == "$":
                out.append("$$")
            else:
                out.append(escapes[value[i + 1]])
            i += 2
            continue
        if c == '"':
            return "".join(out), i
        out.append(c)
        i += 1
    raise PreflightError(f"{where}: unterminated double quote")


_NAME = re.compile(r"[A-Za-z_][A-Za-z0-9_]*")


def _lookup(name: str, earlier: dict[str, str]) -> tuple[bool, str]:
    """Compose precedence: a variable in the shell wins over the file."""
    if name in os.environ:
        return True, os.environ[name]
    if name in earlier:
        return True, earlier[name]
    return False, ""


def _interpolate(text: str, earlier: dict[str, str], where: str) -> str:
    """Expand $VAR, ${VAR}, ${VAR:-d}, ${VAR-d}, ${VAR:?e}, ${VAR?e} and $$
    as Compose does, with balanced nesting inside a default or error, or
    refuse by name. Command substitution is not dotenv."""
    if "$(" in text or "`" in text:
        raise PreflightError(f"{where}: command substitution is not dotenv; write the value out")
    out = []
    i = 0
    n = len(text)
    while i < n:
        c = text[i]
        if c != "$":
            out.append(c)
            i += 1
            continue
        if i + 1 < n and text[i + 1] == "$":
            out.append("$")
            i += 2
            continue
        if i + 1 < n and text[i + 1] == "{":
            depth = 0
            k = i + 1
            while k < n:
                if text[k] == "{":
                    depth += 1
                elif text[k] == "}":
                    depth -= 1
                    if depth == 0:
                        break
                k += 1
            if k >= n:
                raise PreflightError(f"{where}: unterminated ${{ in {text!r}")
            out.append(_expand_braced(text[i + 2 : k], earlier, where))
            i = k + 1
            continue
        m = _NAME.match(text, i + 1)
        if not m:
            raise PreflightError(f"{where}: a lone $ in {text!r}; write $$ for a literal dollar")
        _, value = _lookup(m.group(0), earlier)
        out.append(value)
        i = m.end()
    return "".join(out)


def _expand_braced(inner: str, earlier: dict[str, str], where: str) -> str:
    m = _NAME.match(inner)
    if not m:
        raise PreflightError(f"{where}: malformed interpolation ${{{inner}}}")
    name = m.group(0)
    rest = inner[m.end() :]
    found, value = _lookup(name, earlier)
    if rest == "":
        return value
    for op in (":-", ":?", "-", "?"):
        if rest.startswith(op):
            arg = _interpolate(rest[len(op) :], earlier, where)
            empty = (not found) if op in ("-", "?") else (not value)
            if op.endswith("-"):
                return arg if empty else value
            if empty:
                raise PreflightError(f"{where}: {name} is required: {arg}")
            return value
    raise PreflightError(f"{where}: unsupported interpolation ${{{inner}}}")


# Store directories under the state root, mirrored by compose.sealed.yaml.
STATE_ROOT_LAYOUT = {
    "data": "data",
    "home": "home",
    "projects": "projects",
    "tmp": "tmp",
    "gh-config": "gh",
}


def effective_env(path: str) -> dict[str, str]:
    """The configuration Compose will see: the file's values, each replaced
    by the shell's when the shell also sets it."""
    env = parse_env_file(path)
    return {k: os.environ.get(k, v) for k, v in env.items()} | {k: v for k, v in os.environ.items() if k.startswith("NOISEDECK_") or k == "COMPOSE_PROJECT_NAME"}


def swap_policy_from_env(env: dict[str, str]) -> str:
    return env.get("NOISEDECK_SEALED_SWAP_POLICY") or env.get("NOISEDECK_SWAP_POLICY") or "personal"


def stores_from_env(env: dict[str, str]) -> dict[str, str]:
    root = env.get("NOISEDECK_SEALED_STATE_ROOT", "")
    if not root:
        raise PreflightError("NOISEDECK_SEALED_STATE_ROOT is not set in the env file")
    root = os.path.abspath(root)
    return {name: os.path.join(root, sub) for name, sub in STATE_ROOT_LAYOUT.items()}


def _parse_stores(items: list[str]) -> dict[str, str]:
    stores: dict[str, str] = {}
    for item in items:
        if "=" not in item:
            raise PreflightError(f"--store expects name=<host path>, got {item!r}")
        name, path = item.split("=", 1)
        if name not in CONTAINER_PATHS:
            raise PreflightError(f"unknown store {name!r}; known: {', '.join(CONTAINER_PATHS)}")
        stores[name] = os.path.abspath(path)
    return stores


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    sub = parser.add_subparsers(dest="command", required=True)

    common = argparse.ArgumentParser(add_help=False)
    common.add_argument("--env-file", default=None, help="Compose dotenv file; supplies the profile, state root, uid and report dir")
    common.add_argument("--swap-policy", default=None, choices=("personal", "paranoid"))
    common.add_argument("--profile", default=None, choices=(PROFILE_SHARED, PROFILE_OWN))
    common.add_argument("--store", action="append", default=[], help="name=<host path>; repeat per store (or derive from --env-file)")
    common.add_argument("--partner-uid", type=int, default=None)

    sub.add_parser("qualify", parents=[common], help="qualify the host sources before the container exists")
    rep = sub.add_parser("report", parents=[common], help="verify the container's mounts and write the report")
    rep.add_argument("--container", required=True, help="container name or id")
    rep.add_argument("--report-dir", default=None, type=Path)
    rep.add_argument("--pid", type=int, default=None, help="container init pid (default: docker inspect)")
    rep.add_argument("--container-id", default=None, help="configured container hostname (default: inspect through --engine)")
    rep.add_argument("--engine", default="docker", choices=("docker", "podman"), help="the engine that launched the container; inspection goes through the same one")
    env_cmd = sub.add_parser("env", help="print one value from a Compose dotenv file, parsed without a shell")
    env_cmd.add_argument("--env-file", required=True)
    env_cmd.add_argument("key")

    args = parser.parse_args(argv)
    host = Host()
    try:
        if args.command == "env":
            values = effective_env(args.env_file)
            if args.key not in values:
                return 3
            print(values[args.key])
            return 0
        env = effective_env(args.env_file) if args.env_file else {}
        profile = args.profile or env.get("NOISEDECK_SEALED_PROFILE") or PROFILE_SHARED
        partner_uid = args.partner_uid
        if partner_uid is None and args.env_file:
            # Under rootless Podman with keep-id the invoking user is the
            # mount owner and becomes the container's runtime uid; the
            # container-side uid in the env file is a namespace number, not
            # a host identity.
            partner_uid = os.getuid()
        stores = _parse_stores(args.store) if args.store else stores_from_env(env)
        swap_policy = args.swap_policy or swap_policy_from_env(env)
        q = qualify(host, profile, stores, partner_uid, swap_policy)
        for name, c in sorted(q.stores.items()):
            print(f"{name:10} {q.host_paths[name]:40} {c.backing}")
        if args.command == "qualify":
            print(f"qualified: profile {profile}, swap policy {swap_policy}")
            return 0
        report_dir = args.report_dir or (Path(env["NOISEDECK_SEALED_REPORT_DIR"]) if env.get("NOISEDECK_SEALED_REPORT_DIR") else None)
        if report_dir is None:
            raise PreflightError("--report-dir or NOISEDECK_SEALED_REPORT_DIR is required")
        pid = args.pid if args.pid is not None else container_init_pid(args.engine, args.container)
        cid = args.container_id if args.container_id else container_hostname(args.engine, args.container)
        report = build_report(host, q, pid, cid)
        path = write_report(report, report_dir)
        print(f"report written to {path} for container {cid}")
        return 0
    except PreflightError as exc:
        print(f"sealed-preflight: refusing: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
