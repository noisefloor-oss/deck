#!/bin/sh
set -eu
umask 077

# NSS needs writable databases even when every imported trust decision is fixed.
browser_home=$(mktemp -d /tmp/noise-browser-home.XXXXXX)
mkdir -p "${browser_home}/.pki/nssdb"
cp /opt/noise-browser/nssdb/cert9.db /opt/noise-browser/nssdb/key4.db "${browser_home}/.pki/nssdb/"
chmod 0600 "${browser_home}/.pki/nssdb/"*.db
exec env HOME="${browser_home}" "$@"
