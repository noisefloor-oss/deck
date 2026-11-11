#!/bin/sh
# Build the pinned confined-linux-v1 Bubblewrap executable for linux/amd64.
# Any other architecture fails closed.
#
# What the script pins: the four fetched archives — Bubblewrap, libcap, GNU
# make, and Zig — are verified by SHA-256 before use, and the bwrap sources
# are compiled and linked only by the pinned Zig release, which supplies the
# cross compiler, linker, musl 1.2.5, and kernel UAPI headers. Optimization
# is fixed (-O2 -g0) and SOURCE_DATE_EPOCH is fixed. The executable links
# statically (-static: no PT_INTERP, no DT_NEEDED, both checked below), the
# link discards the symbol table (-s), debug info is omitted (-g0), and a
# strings check rejects any absolute build path that still reached the
# bytes.
#
# What the script does not pin: the builder container supplies the ambient
# tools the recipe runs on (POSIX shell utilities, python3, curl, GNU tar,
# binutils) and the host C compiler and binutils that bootstrap GNU make
# and build libcap's host-side capability-name generator (BUILD_CC, AR,
# RANLIB). Those inputs are pinned only by the digest-pinned builder image
# referenced in release acceptance, not by this script; the output pins
# below are proven inside that image, not on an arbitrary toolchain.
#
# Usage: build-bwrap.sh amd64 <workdir>
#
# Output (under <workdir>/out):
#   rootfs/usr/local/libexec/noise/bwrap  staged install target
#   bwrap-amd64.tar                       deterministic root:root packaging
#
# Three digests, never interchangeable: BUBBLEWRAP_SHA256 is the upstream
# source archive (provenance only), EXPECTED_EXECUTABLE_SHA256_AMD64 is the
# executable identity, and EXPECTED_ROOTFS_TAR_SHA256_AMD64 is the packaged
# rootfs tar. The script prints `executable-sha256 amd64 <hex>` and
# `rootfs-tar-sha256 amd64 <hex>` and exits non-zero when a reproduced
# digest differs from its pin. The stat and tar checks below assert the
# staged artifact and the packaged member only; the installed file on a
# supervisor host — path, mode, no symlink — is verified by host
# admission, not here.

set -eu

ARCH=${1:?usage: build-bwrap.sh amd64 <workdir>}
WORK=${2:?usage: build-bwrap.sh amd64 <workdir>}

BUBBLEWRAP_VERSION=0.11.2
BUBBLEWRAP_SHA256=69abc30005d2186baf7737feacd8da35633b93cf5af38838ecff17c5f8e924f6
LIBCAP_VERSION=2.71
LIBCAP_SHA256=b7006c9af5168315f35fc734bf1a8d2aa70766bd8b8c4340962e05b19c35b900
MAKE_VERSION=4.4.1
MAKE_SHA256=dd16fb1d67bfab79a72f5e8390735c49e3e8e70b4945a15ab1f81ddb78658fb3
ZIG_VERSION=0.14.1
ZIG_SHA256=24aeeec8af16c381934a6cd7d95c807a8cb2cf7df9fa40d359aa884195c4716c

# Proven outputs of two independent clean builds inside the digest-pinned
# builder container. They must stay identical to internal/supervisor's
# closed GOARCH pin; the deploy packaging contract test enforces the
# coherence.
EXPECTED_EXECUTABLE_SHA256_AMD64=7710b19d89bedbdd3b1dd8ac46643f188f8fdc92917fe3093abd827eb783cf7f
EXPECTED_ROOTFS_TAR_SHA256_AMD64=f61874a5bd6e02d89ffd0022f0bbd486a74948325923e7c8cac700b604501344

if [ "$ARCH" != amd64 ]; then
  echo "build-bwrap: unsupported architecture: $ARCH" >&2
  exit 64
fi
TRIPLET=x86_64-linux-musl
ELF_MACHINE='Advanced Micro Devices X86-64'
HOST_MACHINE=x86_64

# Fixed deterministic build environment. SOURCE_DATE_EPOCH pins any __DATE__
# or __TIME__ expansion and the packaged mtimes.
SOURCE_DATE_EPOCH=1735689600
export SOURCE_DATE_EPOCH LC_ALL=C LANG=C TZ=UTC
umask 022

SRC="$WORK/src"
BOOT="$WORK/bootstrap"
SYS="$WORK/sysroot-$ARCH"
OUT="$WORK/out"
rm -rf "$BOOT" "$SYS" "$OUT"
mkdir -p "$SRC" "$BOOT" "$SYS" "$OUT/rootfs" "$OUT/bin" "$TMPDIR"

fetch() {
  max_time=$1; retries=$2; url=$3; want=$4; shift 4; file="$SRC/$(basename "$url")"
  if [ ! -f "$file" ]; then
    fetched=
    for candidate in "$url" "$@"; do
      if curl --fail --location --silent --show-error --retry "$retries" --retry-all-errors \
        --connect-timeout 10 --max-time "$max_time" "$candidate" --output "$file"; then
        fetched=1
        break
      fi
      rm -f "$file"
    done
    [ -n "$fetched" ] || return 1
  fi
  echo "$want  $file" | sha256sum --check --strict
}

extract() {
  python3 - "$1" "$2" <<'PY'
import sys, tarfile
with tarfile.open(sys.argv[1]) as archive:
    archive.extractall(sys.argv[2])
PY
}

# Debian ships this release's tarball unmodified, so its pool is a genuine
# second source. Do not generalise that to the other archives: Debian has no
# libcap at this version at all - its packaging skips 2.66 to 2.73 - so the
# pool below is a per-project answer, not a per-distro one. Each fallback is
# verified against its own hash rather than assumed from the pattern.
fetch 90 2 "https://github.com/containers/bubblewrap/releases/download/v${BUBBLEWRAP_VERSION}/bubblewrap-${BUBBLEWRAP_VERSION}.tar.xz" "$BUBBLEWRAP_SHA256" \
  "https://deb.debian.org/debian/pool/main/b/bubblewrap/bubblewrap_${BUBBLEWRAP_VERSION}.orig.tar.xz"
# The second fallback is deliberately off kernel.org's own infrastructure.
# kernel.org redirects to mirrors.edge.kernel.org, so a "different"
# kernel.org hostname is the same endpoint and fails in the same breath -
# a CI run has already been observed timing out on that redirect target.
# mirrorservice.org is a separate site that carries ftp.kernel.org's tree,
# and it serves these exact bytes.
fetch 90 2 "https://kernel.org/pub/linux/libs/security/linux-privs/libcap2/libcap-${LIBCAP_VERSION}.tar.xz" "$LIBCAP_SHA256" \
  "https://cdn.kernel.org/pub/linux/libs/security/linux-privs/libcap2/libcap-${LIBCAP_VERSION}.tar.xz" \
  "https://www.mirrorservice.org/sites/ftp.kernel.org/pub/linux/libs/security/linux-privs/libcap2/libcap-${LIBCAP_VERSION}.tar.xz"
fetch 90 2 "https://ftp.gnu.org/gnu/make/make-${MAKE_VERSION}.tar.gz" "$MAKE_SHA256" \
  "https://mirrors.ocf.berkeley.edu/gnu/make/make-${MAKE_VERSION}.tar.gz" \
  "https://mirror.csclub.uwaterloo.ca/gnu/make/make-${MAKE_VERSION}.tar.gz"
# Zig is about 49 MB: at the observed 170 KB/s it needs nearly five minutes.
# Give each source one complete transfer rather than repeatedly discarding a
# healthy partial download at 90 seconds. Hash verification remains mandatory.
# The workflow still bounds the entire two-build proof to 15 minutes.
fetch 300 0 "https://ziglang.org/download/${ZIG_VERSION}/zig-x86_64-linux-${ZIG_VERSION}.tar.xz" "$ZIG_SHA256" \
  "https://pkg.machengine.org/zig/zig-x86_64-linux-${ZIG_VERSION}.tar.xz"

# Bootstrap GNU make for libcap's build. The pinned release ships build.sh so
# the bootstrap needs nothing but a POSIX shell, a C compiler, and a C
# library. --disable-dependency-tracking keeps configure's makefile-fragment
# bootstrap from needing an existing make.
extract "$SRC/make-$MAKE_VERSION.tar.gz" "$BOOT"
( cd "$BOOT/make-$MAKE_VERSION" \
  && ./configure --without-guile --disable-dependency-tracking >/dev/null \
  && bash ./build.sh >/dev/null )
MAKE="$BOOT/make-$MAKE_VERSION/make"

extract "$SRC/zig-x86_64-linux-$ZIG_VERSION.tar.xz" "$BOOT"
ZIG="$BOOT/zig-x86_64-linux-$ZIG_VERSION/zig"
extract "$SRC/libcap-$LIBCAP_VERSION.tar.xz" "$BOOT"
extract "$SRC/bubblewrap-$BUBBLEWRAP_VERSION.tar.xz" "$BOOT"

# Compiler front end for the single supported target.
CC="$OUT/bin/$TRIPLET-cc"
{
  echo "#!/bin/sh"
  echo "exec \"$ZIG\" cc -target $TRIPLET \"\$@\""
} > "$CC"
chmod 0555 "$CC"

# libcap supplies the <sys/capability.h> header bwrap includes and the
# cap_from_name call behind its --cap-add/--cap-drop parsing, so libcap
# object code is statically linked into the executable from the installed
# archive. install-static-cap installs exactly libcap.a, that header, and
# libcap's pkg-config file into the sysroot. USE_GPERF=no builds the
# capability name table with libcap's own _makenames generator instead of
# requiring gperf; BUILD_CC compiles the generator, which reads the
# archive's bundled kernel UAPI header.
( cd "$BOOT/libcap-$LIBCAP_VERSION" \
  && "$MAKE" -C libcap install-static-cap \
      CC="$CC" BUILD_CC="${BUILD_CC:-cc}" AR="${AR:-ar}" RANLIB="${RANLIB:-ranlib}" \
      USE_GPERF=no prefix="$SYS" lib=lib >/dev/null )

BW="$BOOT/bubblewrap-$BUBBLEWRAP_VERSION"
OBJ="$OUT/obj"; mkdir -p "$OBJ"

# Fixed compile list mirroring the upstream meson build for a plain release:
# the generated config.h is the only define the sources read. The warning
# set is fixed rather than compiler-probed and carries upstream meson's
# warning flags minus -Werror=parenthesis and the conditionally probed
# format trio (-Werror=format=2, -Werror=format-security,
# -Werror=format-nonliteral). -O2 fixes optimization, -g0 omits debug
# info, and the -s link discards the symbol table.
cat > "$OBJ/config.h" <<EOF
#define PACKAGE_STRING "bubblewrap $BUBBLEWRAP_VERSION"
EOF

WARNINGS="-Wall -Wextra -Werror=shadow -Werror=empty-body -Werror=strict-prototypes \
-Werror=missing-prototypes -Werror=implicit-function-declaration -Werror=pointer-arith \
-Werror=init-self -Werror=missing-declarations -Werror=return-type -Werror=overflow \
-Werror=int-conversion -Werror=incompatible-pointer-types -Werror=misleading-indentation \
-Werror=missing-include-dirs -Werror=aggregate-return -Werror=switch-default -Wswitch-enum \
-Wno-missing-field-initializers -Wno-error=missing-field-initializers"

for src in bubblewrap bind-mount network utils; do
  "$CC" -I"$OBJ" -I"$BW" -I"$SYS/include" -D_GNU_SOURCE -D_FILE_OFFSET_BITS=64 \
    $WARNINGS -O2 -g0 -c "$BW/$src.c" -o "$OBJ/$src.o"
done

"$CC" -s -o "$OBJ/bwrap" "$OBJ/bubblewrap.o" "$OBJ/bind-mount.o" "$OBJ/network.o" "$OBJ/utils.o" \
  -Wl,--as-needed -Wl,--no-undefined -static -O2 -g0 "$SYS/lib/libcap.a"

# Stage the install target confined-linux-v1 requires: a regular file (no
# symlink) at /usr/local/libexec/noise/bwrap with mode 0555 and link count
# 1. The checks assert the staged artifact and the archive entry; installed
# host state is verified at host admission.
TARGET=rootfs/usr/local/libexec/noise/bwrap
install -d "$OUT/rootfs/usr/local/libexec/noise"
install -m 0555 "$OBJ/bwrap" "$OUT/$TARGET"

fail() { echo "build-bwrap: $1" >&2; exit 1; }

[ "$(stat -c %F "$OUT/$TARGET")" = "regular file" ] || fail "target is not a regular file"
[ "$(stat -c %h "$OUT/$TARGET")" = "1" ] || fail "target has more than one hard link"
[ "$(stat -c %a "$OUT/$TARGET")" = "555" ] || fail "target mode is not 0555"

# Identity and static-linkage proofs.
MACHINE=$(readelf -h "$OUT/$TARGET" | sed -n 's/^ *Machine: *//p' | head -1)
[ "$MACHINE" = "$ELF_MACHINE" ] || fail "wrong ELF machine: $MACHINE"
readelf -l "$OUT/$TARGET" | grep -q INTERP && fail "PT_INTERP present"
readelf -d "$OUT/$TARGET" | grep -q NEEDED && fail "DT_NEEDED present"
if strings "$OUT/$TARGET" | grep -qF "$WORK"; then
  fail "build path leaked into executable"
fi
VERSION_STRING="bubblewrap $BUBBLEWRAP_VERSION"
strings "$OUT/$TARGET" | grep -qxF "$VERSION_STRING" || fail "embedded version string missing"
if [ "$(uname -m)" = "$HOST_MACHINE" ]; then
  [ "$("$OUT/$TARGET" --version)" = "$VERSION_STRING" ] || fail "executed version mismatch"
  ldd "$OUT/$TARGET" 2>&1 | grep -q 'not a dynamic executable' || fail "ldd reports dynamic linkage"
fi

# Packaging: the archive carries the parent directory hierarchy plus the
# one payload entry — the staged executable — owned root:root with the
# pinned mtime, so archive ownership is meaningful without a privileged
# build. GNU tar records the payload member as mode 0555 with uid/gid 0;
# ustar has no link-count field, so the nlink check above asserts the
# staged file only.
tar --sort=name --format=ustar --owner=0 --group=0 --numeric-owner \
  --mtime="@$SOURCE_DATE_EPOCH" -cf "$OUT/bwrap-$ARCH.tar" -C "$OUT/rootfs" usr

# Tar member proof: the payload entry exists once, is a regular file with
# mode 0555 and uid/gid 0, and every other entry is a directory of the
# hierarchy placing it.
python3 - "$OUT/bwrap-$ARCH.tar" <<'PY'
import sys, tarfile

payload = "usr/local/libexec/noise/bwrap"
with tarfile.open(sys.argv[1]) as archive:
    members = archive.getmembers()
    if [m.name for m in members].count(payload) != 1:
        raise SystemExit(f"archive does not carry exactly one {payload} member")
    if any(not m.isdir() for m in members if m.name != payload):
        raise SystemExit("archive carries a non-directory entry besides the payload")
    member = archive.getmember(payload)
    if not member.isfile():
        raise SystemExit(f"payload {payload} is not a regular-file member")
    if member.mode != 0o555 or member.uid != 0 or member.gid != 0:
        raise SystemExit(f"payload {payload} is not root:root mode 0555")
PY

TARSHA256=$(sha256sum "$OUT/bwrap-$ARCH.tar" | awk '{print $1}')
[ "$TARSHA256" = "$EXPECTED_ROOTFS_TAR_SHA256_AMD64" ] \
  || fail "packaged rootfs tar digest $TARSHA256 does not match the pin"
SHA256=$(sha256sum "$OUT/$TARGET" | awk '{print $1}')
[ "$SHA256" = "$EXPECTED_EXECUTABLE_SHA256_AMD64" ] \
  || fail "reproduced digest $SHA256 does not match the pin"
printf 'executable-sha256 %s %s\n' "$ARCH" "$SHA256"
printf 'rootfs-tar-sha256 %s %s\n' "$ARCH" "$TARSHA256"
