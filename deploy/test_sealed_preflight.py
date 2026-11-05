"""Fixture-driven tests for the host-side sealed-state preflight."""

from __future__ import annotations

import importlib.util
import json
import os
import sys
import tempfile
import unittest
import unittest.mock
from pathlib import Path

HERE = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("sealed_preflight", HERE / "sealed-preflight.py")
sp = importlib.util.module_from_spec(SPEC)
sys.modules["sealed_preflight"] = sp
SPEC.loader.exec_module(sp)


class FixtureHost(sp.Host):
    """A Host whose /proc and /sys are directories the test builds."""

    def __init__(self, root: Path):
        super().__init__(proc=root / "proc", sys=root / "sys")
        self._root = root
        self._devices: dict[str, str] = {}

    def device_for_path(self, path: str) -> str:
        return self._devices.get(path, "0:0")

    def source_device(self, source: str) -> str:
        return self._devices.get(source, "")


def write(path: Path, text: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)


class Fixture:
    """Builds a host: mountinfo, sysfs device-mapper uuids, btrfs device
    lists, the fuse parameter, swaps and the resume device."""

    def __init__(self, tmp: Path):
        self.root = tmp
        self.host = FixtureHost(tmp)
        self.mountinfo: list[str] = ["1 0 0:1 / / rw - ext4 /dev/sda1 rw"]
        self.container_mountinfo: dict[int, list[str]] = {}
        write(tmp / "sys" / "module" / "fuse" / "parameters" / "allow_sys_admin_access", "N\n")
        write(tmp / "proc" / "swaps", "Filename\tType\tSize\tUsed\tPriority\n")
        write(tmp / "sys" / "power" / "resume", "0:0\n")
        write(tmp / "proc" / "sys" / "kernel" / "random" / "boot_id", "11111111-2222-3333-4444-555555555555\n")
        for name in ("data", "home", "projects", "tmp"):
            (tmp / "host" / name).mkdir(parents=True, exist_ok=True)

    def host_path(self, name: str) -> str:
        return str(self.root / "host" / name)

    def mount(self, mount_id: int, device: str, root: str, point: str, fstype: str, source: str, opts: str = "rw") -> None:
        self.mountinfo.append(f"{mount_id} 1 {device} {root} {point} rw,relatime - {fstype} {source} {opts}")

    def dm_crypt(self, device: str, name: str = "cryptroot") -> None:
        write(self.root / "sys" / "dev" / "block" / device / "dm" / "uuid", f"CRYPT-LUKS2-0000-{name}\n")

    def dm_plain(self, device: str) -> None:
        write(self.root / "sys" / "dev" / "block" / device / "dm" / "uuid", "LVM-0000\n")

    def btrfs(self, fs_uuid: str, source: str, devices: dict[str, str]) -> None:
        """devices maps sysfs kernel names to major:minor; the mount source
        node is the first of them, as on a real host where /dev/mapper/x is
        a link to dm-N."""
        base = self.root / "sys" / "fs" / "btrfs" / fs_uuid / "devices"
        base.mkdir(parents=True, exist_ok=True)
        for name, majmin in devices.items():
            (base / name).mkdir(exist_ok=True)
            write(self.root / "sys" / "class" / "block" / name / "dev", majmin + "\n")
        self.host._devices[source] = next(iter(devices.values()))

    def swap(self, path: str, device: str) -> None:
        text = (self.root / "proc" / "swaps").read_text()
        write(self.root / "proc" / "swaps", text + f"{path}\tpartition\t8388604\t0\t100\n")
        base = os.path.basename(path)
        if path.startswith("/dev/mapper/"):
            kernel = "dm-" + device.split(":")[1]
            write(self.root / "sys/class/block" / kernel / "dm/name", base + "\n")
            base = kernel
        write(self.root / "sys" / "class" / "block" / base / "dev", device + "\n")

    def resume(self, value: str) -> None:
        write(self.root / "sys" / "power" / "resume", value + "\n")

    def fuse_admin(self, value: str) -> None:
        write(self.root / "sys" / "module" / "fuse" / "parameters" / "allow_sys_admin_access", value + "\n")

    def container(self, pid: int, lines: list[str]) -> None:
        write(self.root / "proc" / str(pid) / "mountinfo", "\n".join(lines) + "\n")

    def flush(self) -> None:
        write(self.root / "proc" / "self" / "mountinfo", "\n".join(self.mountinfo) + "\n")

    def stores(self) -> dict[str, str]:
        return {name: self.host_path(name) for name in ("data", "home", "projects", "tmp")}


class PreflightTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix="sealed-preflight-"))
        self.fx = Fixture(self.tmp)

    def tearDown(self):
        import shutil

        shutil.rmtree(self.tmp, ignore_errors=True)

    # --- own-host: whole-disk LUKS -------------------------------------

    def luks_host(self, encrypted: bool = True) -> Fixture:
        fx = self.fx
        fx.mount(10, "253:0", "/", str(fx.root / "host"), "ext4", "/dev/mapper/cryptroot")
        if encrypted:
            fx.dm_crypt("253:0")
        else:
            fx.dm_plain("253:0")
        fx.flush()
        return fx

    def test_own_host_luks_qualifies(self):
        fx = self.luks_host()
        q = sp.qualify(fx.host, sp.PROFILE_OWN, fx.stores(), None)
        for name in ("data", "home", "projects", "tmp"):
            self.assertEqual(q.stores[name].backing, sp.BACKING_DM_CRYPT, name)
            self.assertEqual(q.stores[name].root, "/" + name)

    def test_own_host_plain_dm_target_refuses_naming_the_path(self):
        fx = self.luks_host(encrypted=False)
        with self.assertRaises(sp.PreflightError) as ctx:
            sp.qualify(fx.host, sp.PROFILE_OWN, fx.stores(), None)
        self.assertIn(fx.host_path("data"), str(ctx.exception))
        self.assertIn("not dm-crypt", str(ctx.exception))

    def test_own_host_allows_dm_crypt_swap_but_not_plain_swap(self):
        fx = self.luks_host()
        fx.swap("/dev/mapper/cryptswap", "253:1")
        fx.dm_crypt("253:1", "cryptswap")
        sp.qualify(fx.host, sp.PROFILE_OWN, fx.stores(), None, "paranoid")
        fx.swap("/dev/zram0", "251:0")
        with self.assertRaises(sp.PreflightError) as ctx:
            sp.qualify(fx.host, sp.PROFILE_OWN, fx.stores(), None, "paranoid")
        self.assertIn("swap /dev/zram0 is not verified dm-crypt", str(ctx.exception))

    def test_own_host_tmpfs_tmp_passes(self):
        fx = self.luks_host()
        fx.mount(11, "0:50", "/", fx.host_path("tmp"), "tmpfs", "tmpfs")
        fx.flush()
        q = sp.qualify(fx.host, sp.PROFILE_OWN, fx.stores(), None)
        self.assertEqual(q.stores["tmp"].backing, sp.BACKING_TMPFS)

    # --- btrfs on the host --------------------------------------------

    def test_btrfs_resolves_through_its_device_list(self):
        fx = self.fx
        fx.mount(10, "0:35", "/root", str(fx.root / "host"), "btrfs", "/dev/mapper/cryptdata", "rw,subvol=/root")
        fx.btrfs("0a18df2b-0000", "/dev/mapper/cryptdata", {"dm-0": "253:0"})
        fx.dm_crypt("253:0")
        fx.flush()
        q = sp.qualify(fx.host, sp.PROFILE_OWN, fx.stores(), None)
        self.assertEqual(q.stores["data"].backing, sp.BACKING_DM_CRYPT)
        self.assertEqual(q.stores["data"].root, "/root/data")

    def test_btrfs_on_a_plain_partition_refuses(self):
        fx = self.fx
        fx.mount(10, "0:35", "/root", str(fx.root / "host"), "btrfs", "/dev/nvme0n1p4", "rw,subvol=/root")
        fx.btrfs("0a18df2b-0000", "/dev/nvme0n1p4", {"nvme0n1p4": "259:4"})
        fx.flush()
        with self.assertRaises(sp.PreflightError) as ctx:
            sp.qualify(fx.host, sp.PROFILE_OWN, fx.stores(), None)
        self.assertIn("btrfs device 259:4 is not dm-crypt", str(ctx.exception))

    def test_btrfs_without_a_device_list_is_unresolved(self):
        fx = self.fx
        fx.mount(10, "0:35", "/root", str(fx.root / "host"), "btrfs", "/dev/nvme0n1p4", "rw,subvol=/root")
        fx.flush()
        with self.assertRaises(sp.PreflightError) as ctx:
            sp.qualify(fx.host, sp.PROFILE_OWN, fx.stores(), None)
        self.assertIn("unresolved", str(ctx.exception))

    # --- shared-host: gocryptfs owned by the partner uid ---------------

    def gocryptfs_host(self, opts: str = "rw,user_id=1026,group_id=100,default_permissions") -> Fixture:
        fx = self.fx
        fx.mount(10, "0:200", "/", str(fx.root / "host"), "fuse.gocryptfs", "/dev/fuse", opts)
        fx.flush()
        return fx

    def test_shared_host_gocryptfs_qualifies(self):
        fx = self.gocryptfs_host()
        q = sp.qualify(fx.host, sp.PROFILE_SHARED, fx.stores(), 1026)
        for name in ("data", "home", "projects", "tmp"):
            self.assertEqual(q.stores[name].backing, sp.BACKING_GOCRYPTFS, name)

    def test_shared_host_refuses_allow_other_allow_root_wrong_uid_root_mount(self):
        cases = {
            "rw,user_id=1026,group_id=100,allow_other": "allow_other",
            "rw,user_id=1026,group_id=100,allow_root": "allow_root",
            "rw,user_id=1000,group_id=100": "not the partner uid 1026",
            "rw,user_id=0,group_id=0": "mounted by root",
        }
        for opts, reason in cases.items():
            with self.subTest(opts=opts):
                self.fx = Fixture(self.tmp / opts.replace(",", "_").replace("=", "-"))
                fx = self.gocryptfs_host(opts)
                with self.assertRaises(sp.PreflightError) as ctx:
                    sp.qualify(fx.host, sp.PROFILE_SHARED, fx.stores(), 1026)
                self.assertIn(reason, str(ctx.exception))

    def test_shared_host_refuses_when_kernel_allows_sys_admin_access(self):
        fx = self.gocryptfs_host()
        fx.fuse_admin("Y")
        with self.assertRaises(sp.PreflightError) as ctx:
            sp.qualify(fx.host, sp.PROFILE_SHARED, fx.stores(), 1026)
        self.assertIn("allow_sys_admin_access", str(ctx.exception))

    def test_shared_host_allows_swap_but_refuses_hibernation(self):
        fx = self.gocryptfs_host()
        fx.swap("/dev/mapper/cryptswap", "253:1")
        fx.dm_crypt("253:1", "cryptswap")
        sp.qualify(fx.host, sp.PROFILE_SHARED, fx.stores(), 1026)
        sp.qualify(fx.host, sp.PROFILE_SHARED, fx.stores(), 1026, "paranoid")
        self.fx = Fixture(self.tmp / "resume")
        fx = self.gocryptfs_host()
        fx.resume("253:1")
        with self.assertRaises(sp.PreflightError) as ctx:
            sp.qualify(fx.host, sp.PROFILE_SHARED, fx.stores(), 1026)
        self.assertIn("hibernation", str(ctx.exception))

    def test_paranoid_refuses_missing_unverifiable_and_mixed_swap(self):
        fx = self.gocryptfs_host()
        with self.assertRaisesRegex(sp.PreflightError, "requires active encrypted swap"):
            sp.qualify(fx.host, sp.PROFILE_SHARED, fx.stores(), 1026, "paranoid")
        fx.swap("/dev/dm-1", "253:1")
        with self.assertRaisesRegex(sp.PreflightError, "not verified dm-crypt"):
            sp.qualify(fx.host, sp.PROFILE_SHARED, fx.stores(), 1026, "paranoid")
        fx.dm_crypt("253:1")
        q = sp.qualify(fx.host, sp.PROFILE_SHARED, fx.stores(), 1026, "paranoid")
        self.assertEqual(q.swap_policy, "paranoid")
        fx.swap("/dev/zram0", "251:0")
        with self.assertRaisesRegex(sp.PreflightError, "not verified dm-crypt"):
            sp.qualify(fx.host, sp.PROFILE_SHARED, fx.stores(), 1026, "paranoid")
        sp.qualify(fx.host, sp.PROFILE_SHARED, fx.stores(), 1026, "personal")
        with self.assertRaisesRegex(sp.PreflightError, "unknown swap policy"):
            sp.qualify(fx.host, sp.PROFILE_SHARED, fx.stores(), 1026, "typo")

    def test_swap_observation_cannot_silently_disappear(self):
        fx = self.gocryptfs_host()
        (fx.root / "proc/swaps").unlink()
        with self.assertRaisesRegex(sp.PreflightError, "cannot read kernel swap table"):
            sp.qualify(fx.host, sp.PROFILE_SHARED, fx.stores(), 1026)
        for content in ("", "Filename Type Size Used Priority\nmalformed\n"):
            write(fx.root / "proc/swaps", content)
            with self.assertRaisesRegex(sp.PreflightError, "invalid kernel swap"):
                sp.qualify(fx.host, sp.PROFILE_SHARED, fx.stores(), 1026)

    def test_shared_host_refuses_tmpfs_tmp(self):
        fx = self.gocryptfs_host()
        fx.mount(11, "0:50", "/", fx.host_path("tmp"), "tmpfs", "tmpfs")
        fx.flush()
        with self.assertRaises(sp.PreflightError) as ctx:
            sp.qualify(fx.host, sp.PROFILE_SHARED, fx.stores(), 1026)
        self.assertIn("/proc/<pid>/root", str(ctx.exception))

    def test_shared_host_needs_partner_uid_and_every_required_store(self):
        fx = self.gocryptfs_host()
        with self.assertRaises(sp.PreflightError):
            sp.qualify(fx.host, sp.PROFILE_SHARED, fx.stores(), None)
        stores = fx.stores()
        del stores["tmp"]
        with self.assertRaises(sp.PreflightError) as ctx:
            sp.qualify(fx.host, sp.PROFILE_SHARED, stores, 1026)
        self.assertIn("tmp (/tmp) has no host path", str(ctx.exception))

    # --- marker files and environment are not inputs -------------------

    def test_marker_file_and_environment_are_ignored(self):
        fx = self.luks_host(encrypted=False)
        write(fx.root / "host" / "data" / ".encrypted", "trust me")
        os.environ["NOISEDECK_SEALED_REQUIRE_ENCRYPTED_STATE"] = "false"
        os.environ["NOISEDECK_STATE_IS_ENCRYPTED"] = "yes"
        try:
            with self.assertRaises(sp.PreflightError):
                sp.qualify(fx.host, sp.PROFILE_OWN, fx.stores(), None)
        finally:
            del os.environ["NOISEDECK_SEALED_REQUIRE_ENCRYPTED_STATE"]
            del os.environ["NOISEDECK_STATE_IS_ENCRYPTED"]

    # --- the report --------------------------------------------------

    def container_lines(self, fx: Fixture, device: str = "253:0", ids=(1437, 1441, 1439, 1438)) -> list[str]:
        return [
            "1 0 0:90 / / ro - overlay overlay rw",
            f"{ids[0]} 1 {device} /data /data rw - ext4 /dev/mapper/cryptroot rw",
            f"{ids[1]} 1 {device} /home /home/agent rw - ext4 /dev/mapper/cryptroot rw",
            f"{ids[2]} 1 {device} /projects /workspace rw - ext4 /dev/mapper/cryptroot rw",
            f"{ids[3]} 1 {device} /tmp /tmp rw - ext4 /dev/mapper/cryptroot rw",
        ]

    def test_report_binds_boot_container_and_the_container_mount_view(self):
        fx = self.luks_host()
        fx.container(4242, self.container_lines(fx))
        q = sp.qualify(fx.host, sp.PROFILE_OWN, fx.stores(), None)
        report = sp.build_report(fx.host, q, 4242, "7207dd8c6b40")
        self.assertEqual(report["boot_id"], "11111111-2222-3333-4444-555555555555")
        self.assertEqual(report["container_id"], "7207dd8c6b40")
        self.assertEqual(report["profile"], sp.PROFILE_OWN)
        self.assertEqual(report["swap_policy"], "personal")
        self.assertEqual(report["stores"]["data"], {
            "path": "/data", "mount_id": 1437, "device": "253:0", "root": "/data", "backing": sp.BACKING_DM_CRYPT,
        })
        out = sp.write_report(report, fx.root / "run")
        self.assertEqual(json.loads(out.read_text())["stores"]["tmp"]["mount_id"], 1438)
        self.assertEqual(oct(out.stat().st_mode & 0o777), "0o444")

    def test_report_refuses_a_container_mount_from_an_unqualified_source(self):
        fx = self.luks_host()
        lines = self.container_lines(fx)
        lines[1] = "1437 1 8:1 /data /data rw - ext4 /dev/sda1 rw"
        fx.container(4242, lines)
        q = sp.qualify(fx.host, sp.PROFILE_OWN, fx.stores(), None)
        with self.assertRaises(sp.PreflightError) as ctx:
            sp.build_report(fx.host, q, 4242, "7207dd8c6b40")
        self.assertIn("data (/data): container mount is 8:1", str(ctx.exception))
        self.assertIn("no qualified host source matches", str(ctx.exception))

    def test_report_refuses_a_container_mount_rooted_elsewhere(self):
        fx = self.luks_host()
        lines = self.container_lines(fx)
        lines[1] = "1437 1 253:0 /elsewhere /data rw - ext4 /dev/mapper/cryptroot rw"
        fx.container(4242, lines)
        q = sp.qualify(fx.host, sp.PROFILE_OWN, fx.stores(), None)
        with self.assertRaises(sp.PreflightError) as ctx:
            sp.build_report(fx.host, q, 4242, "7207dd8c6b40")
        self.assertIn("root /elsewhere", str(ctx.exception))

    def test_report_refuses_a_store_that_is_not_mounted_in_the_container(self):
        fx = self.luks_host()
        fx.container(4242, self.container_lines(fx)[:4])
        q = sp.qualify(fx.host, sp.PROFILE_OWN, fx.stores(), None)
        with self.assertRaises(sp.PreflightError) as ctx:
            sp.build_report(fx.host, q, 4242, "7207dd8c6b40")
        self.assertIn("tmp (/tmp)", str(ctx.exception))

    # --- profile allowlist -----------------------------------------------

    def test_shared_host_refuses_dm_crypt(self):
        fx = self.luks_host()
        with self.assertRaises(sp.PreflightError) as ctx:
            sp.qualify(fx.host, sp.PROFILE_SHARED, fx.stores(), 1026)
        self.assertIn("dm-crypt is not an accepted backing on the shared-host profile", str(ctx.exception))

    def test_own_host_refuses_tmpfs_outside_tmp(self):
        fx = self.luks_host()
        fx.mount(11, "0:50", "/", fx.host_path("data"), "tmpfs", "tmpfs")
        fx.flush()
        with self.assertRaises(sp.PreflightError) as ctx:
            sp.qualify(fx.host, sp.PROFILE_OWN, fx.stores(), None)
        self.assertIn("tmpfs is accepted only for the tmp store", str(ctx.exception))

    # --- nested mounts -----------------------------------------------------

    def test_plaintext_bind_nested_in_a_sealed_store_refuses(self):
        fx = self.gocryptfs_host()
        fx.mount(20, "8:1", "/cfg", fx.host_path("home") + "/.config", "ext4", "/dev/sda1")
        fx.flush()
        with self.assertRaises(sp.PreflightError) as ctx:
            sp.qualify(fx.host, sp.PROFILE_SHARED, fx.stores(), 1026)
        self.assertIn("home:.config", str(ctx.exception))
        self.assertIn("ext4 on /dev/sda1 is not encrypted", str(ctx.exception))

    def test_sealed_nested_mount_is_qualified_and_reported(self):
        fx = self.luks_host()
        fx.mount(20, "253:0", "/srv/palace", fx.host_path("home") + "/.mempalace", "ext4", "/dev/mapper/cryptroot")
        fx.flush()
        q = sp.qualify(fx.host, sp.PROFILE_OWN, fx.stores(), None)
        self.assertEqual(q.stores["home:.mempalace"].backing, sp.BACKING_DM_CRYPT)
        lines = self.container_lines(fx) + [
            "1450 1441 253:0 /srv/palace /home/agent/.mempalace rw - ext4 /dev/mapper/cryptroot rw",
        ]
        fx.container(4242, lines)
        report = sp.build_report(fx.host, q, 4242, "7207dd8c6b40")
        self.assertEqual(report["stores"]["home:/home/agent/.mempalace"], {
            "path": "/home/agent/.mempalace", "mount_id": 1450, "device": "253:0", "root": "/srv/palace", "backing": sp.BACKING_DM_CRYPT,
        })

    def test_report_refuses_a_container_mount_nested_in_a_store_that_no_qualified_source_matches(self):
        fx = self.luks_host()
        fx.container(4242, self.container_lines(fx) + [
            "1450 1441 8:1 /cfg /home/agent/.config rw - ext4 /dev/sda1 rw",
        ])
        q = sp.qualify(fx.host, sp.PROFILE_OWN, fx.stores(), None)
        with self.assertRaises(sp.PreflightError) as ctx:
            sp.build_report(fx.host, q, 4242, "7207dd8c6b40")
        self.assertIn("home:/home/agent/.config (/home/agent/.config): container mount is 8:1", str(ctx.exception))

    # --- the env file is dotenv, never shell --------------------------------

    def test_env_file_is_parsed_with_compose_semantics(self):
        env_path = self.tmp / "compose.env"
        env_path.write_text(
            "# comment\n"
            "\n"
            "COMPOSE_PROJECT_NAME=partner-deck\n"
            "ROOT=/srv/partner state\n"
            "NOISEDECK_SEALED_STATE_ROOT=${ROOT}/deck\n"
            "NOISEDECK_SEALED_REPORT_DIR=\"/run/user/1026/noisedeck sealed\"\n"
            "NOISEDECK_AUTH_PASSWORD_HASH='$2b$12$abc\\def'\n"
            "export NOISEDECK_RUNTIME_UID=1026\n"
            "QUOTED=\"tab\\there \\$notavar\"\n"
            "TRAILING=value # not part of it\n"
            "DEFAULTED=${UNSET_FOR_TEST:-fallback}\n"
            "DOLLAR=cost$$5\n"
            "SHORT=$ROOT/x\n"
        )
        env = sp.parse_env_file(str(env_path))
        self.assertEqual(env["COMPOSE_PROJECT_NAME"], "partner-deck")
        self.assertEqual(env["NOISEDECK_SEALED_STATE_ROOT"], "/srv/partner state/deck")
        self.assertEqual(env["NOISEDECK_SEALED_REPORT_DIR"], "/run/user/1026/noisedeck sealed")
        self.assertEqual(env["NOISEDECK_AUTH_PASSWORD_HASH"], "$2b$12$abc\\def")
        self.assertEqual(env["NOISEDECK_RUNTIME_UID"], "1026")
        self.assertEqual(env["QUOTED"], "tab\there $notavar")
        self.assertEqual(env["TRAILING"], "value")
        self.assertEqual(env["DEFAULTED"], "fallback")
        self.assertEqual(env["DOLLAR"], "cost$5")
        self.assertEqual(env["SHORT"], "/srv/partner state/x")
        stores = sp.stores_from_env(env)
        self.assertEqual(stores["data"], "/srv/partner state/deck/data")
        self.assertEqual(stores["gh-config"], "/srv/partner state/deck/gh")
        self.assertEqual(set(stores), {"data", "home", "projects", "tmp", "gh-config"})

    def test_env_file_refuses_what_it_cannot_read_the_way_compose_would(self):
        cases = {
            "apostrophe escape in single quotes": "X='/srv/partner\\'s deck'\n",
            "text after a quoted value": "X='a' b\n",
            "unterminated quote": "X=\"abc\n",
            "command substitution": "X=$(id -u)\n",
            "backtick": "X=`id -u`\n",
            "required and missing": "X=${UNSET_FOR_TEST:?set it}\n",
            "malformed key": "1X=a\n",
            "no equals": "JUSTAWORD\n",
            "unsupported escape": "X=\"a\\qb\"\n",
        }
        for name, body in cases.items():
            with self.subTest(name=name):
                env_path = self.tmp / f"{name.replace(' ', '_')}.env"
                env_path.write_text(body)
                with self.assertRaises(sp.PreflightError):
                    sp.parse_env_file(str(env_path))

    def test_interpolation_matches_compose_precedence_and_nesting(self):
        env_path = self.tmp / "compose.env"
        env_path.write_text(
            "ROOT=/file\n"
            "NESTED=${MISSING_FOR_TEST:-$ROOT}/deck\n"
            "DEEP=${ROOT:-${MISSING_FOR_TEST:-/fallback}}/deck\n"
            "DEEPER=${MISSING_FOR_TEST:-${ALSO_MISSING:-/fb}}/x\n"
            "SHELLWINS=${ROOT}/deck\n"
        )
        with unittest.mock.patch.dict(os.environ, {"ROOT": "/shell"}, clear=False):
            env = sp.parse_env_file(str(env_path))
            self.assertEqual(env["SHELLWINS"], "/shell/deck", "a shell variable wins over the file, as Compose resolves it")
            self.assertEqual(env["NESTED"], "/shell/deck")
            self.assertEqual(env["DEEP"], "/shell/deck")
            self.assertEqual(env["DEEPER"], "/fb/x")
        env = sp.parse_env_file(str(env_path))
        self.assertEqual(env["NESTED"], "/file/deck")
        self.assertEqual(env["DEEP"], "/file/deck")

    def test_effective_env_lets_the_shell_override_the_file(self):
        env_path = self.tmp / "compose.env"
        env_path.write_text("NOISEDECK_SEALED_STATE_ROOT=/file/deck\nCOMPOSE_PROJECT_NAME=partner\n")
        with unittest.mock.patch.dict(os.environ, {"NOISEDECK_SEALED_STATE_ROOT": "/shell/deck"}, clear=False):
            env = sp.effective_env(str(env_path))
            self.assertEqual(env["NOISEDECK_SEALED_STATE_ROOT"], "/shell/deck")
            self.assertEqual(sp.stores_from_env(env)["data"], "/shell/deck/data")
            import contextlib
            import io

            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                sp.main(["env", "--env-file", str(env_path), "NOISEDECK_SEALED_STATE_ROOT"])
            self.assertEqual(out.getvalue().strip(), "/shell/deck")

    def test_report_inspects_through_the_engine_that_launched(self):
        calls = []

        def fake_run(argv, **kwargs):
            calls.append(argv)
            class R:  # noqa: N801
                stdout = "4242\n" if "State.Pid" in argv[3] else "abcdef0123456789\n"
            return R()

        with unittest.mock.patch("subprocess.run", side_effect=fake_run):
            self.assertEqual(sp.container_init_pid("docker", "c1"), 4242)
            self.assertEqual(sp.container_hostname("podman", "c1"), "abcdef0123456789")
        self.assertEqual(calls[0][0], "docker")
        self.assertEqual(calls[1][0], "podman")
        self.assertEqual(calls[1][3], "{{.Config.Hostname}}")

    def test_env_file_without_a_state_root_refuses(self):
        with self.assertRaises(sp.PreflightError):
            sp.stores_from_env({"COMPOSE_PROJECT_NAME": "x"})

    def test_env_subcommand_prints_one_value_and_exits_3_when_missing(self):
        env_path = self.tmp / "compose.env"
        env_path.write_text("A='with space and a # not comment'\n")
        import contextlib
        import io

        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            code = sp.main(["env", "--env-file", str(env_path), "A"])
        self.assertEqual(code, 0)
        self.assertEqual(out.getvalue(), "with space and a # not comment\n")
        self.assertEqual(sp.main(["env", "--env-file", str(env_path), "MISSING"]), 3)

    def test_qualify_reads_the_layout_from_the_env_file(self):
        fx = self.gocryptfs_host()
        root = fx.root / "host"
        for sub in ("gh",):
            (root / sub).mkdir(exist_ok=True)
        env_path = fx.root / "compose.env"
        env_path.write_text(
            f"NOISEDECK_SEALED_STATE_ROOT={root}\n"
            "NOISEDECK_SEALED_PROFILE=shared-host\n"
            "NOISEDECK_RUNTIME_UID=1026\n"
        )
        env = sp.parse_env_file(str(env_path))
        q = sp.qualify(fx.host, env["NOISEDECK_SEALED_PROFILE"], sp.stores_from_env(env), int(env["NOISEDECK_RUNTIME_UID"]))
        self.assertEqual(q.stores["gh-config"].backing, sp.BACKING_GOCRYPTFS)

    def test_swap_policy_environment_prefers_canonical_then_legacy(self):
        self.assertEqual(sp.swap_policy_from_env({"NOISEDECK_SWAP_POLICY": "paranoid"}), "paranoid")
        self.assertEqual(sp.swap_policy_from_env({
            "NOISEDECK_SWAP_POLICY": "paranoid",
            "NOISEDECK_SEALED_SWAP_POLICY": "personal",
        }), "personal")

    def test_mountinfo_parser_handles_escapes_and_longest_prefix(self):
        mounts = sp.parse_mountinfo(
            "1 0 0:1 / / rw - ext4 /dev/sda1 rw\n"
            "2 1 0:2 / /mnt/with\\040space rw - tmpfs tmpfs rw\n"
            "3 1 0:3 / /mnt rw - ext4 /dev/sdb1 rw\n"
        )
        self.assertEqual(mounts[1].mount_point, "/mnt/with space")
        self.assertEqual(sp.mount_for(mounts, "/mnt/with space/file").id, 2)
        self.assertEqual(sp.mount_for(mounts, "/mnt/other").id, 3)
        self.assertEqual(sp.mount_for(mounts, "/etc").id, 1)


if __name__ == "__main__":
    unittest.main()
