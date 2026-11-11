#!/bin/sh
set -eu

# Change only the Ubuntu transport endpoints. Suite selection, archive keys
# and APT signature verification stay with the distribution configuration.
root=${1:-/etc/apt}
for source in "$root/apt-mirrors.txt" "$root/apt-mirrors-security.txt" "$root/sources.list" "$root/sources.list.d/"*.list "$root/sources.list.d/"*.sources; do
  [ -f "$source" ] || continue
  sed -E -i \
    -e 's#https?://ports\.ubuntu\.com/ubuntu-ports([/[:space:]]|$)#https://mirrors.mit.edu/ubuntu-ports\1#g' \
    -e 's#https?://(azure\.archive|archive|security)\.ubuntu\.com/ubuntu([/[:space:]]|$)#https://mirrors.mit.edu/ubuntu\2#g' \
    "$source"
done
