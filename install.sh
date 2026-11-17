#!/bin/sh
set -eu

repo='smmya/fsend'
die() { printf '%s\n' "fsend: $*" >&2; exit 1; }
command -v curl >/dev/null 2>&1 || die 'curl is required'
command -v tar >/dev/null 2>&1 || die 'tar is required'
case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) die 'Supported systems: Linux, macOS. Use install.ps1 on Windows.' ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  armv7l) arch=armv7 ;;
  *) die 'Unsupported CPU architecture' ;;
esac
[ "$os-$arch" != darwin-armv7 ] || die 'Unsupported macOS CPU architecture'
fetch() { curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 --connect-timeout 15 --max-time 300 --retry 3 "$@"; }
version=${FSEND_VERSION:-}
if [ -z "$version" ]; then
  release_url=$(fetch -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest")
  case "$release_url" in "https://github.com/$repo/releases/tag/"*) version=${release_url##*/} ;; *) die 'Cannot resolve latest release' ;; esac
fi
printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$' || die 'Invalid FSEND_VERSION (expected vX.Y.Z)'
asset="fsend_${version#v}_${os}_${arch}.tar.gz"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
base="https://github.com/$repo/releases/download/$version"
fetch "$base/$asset" -o "$tmp/$asset"
fetch "$base/SHA256SUMS" -o "$tmp/SHA256SUMS"
expected=$(awk -v name="$asset" '$2 == name {print $1}' "$tmp/SHA256SUMS")
printf '%s\n' "$expected" | grep -Eq '^[0-9a-f]{64}$' || die 'Checksum entry missing or invalid'
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp/$asset" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')
else
  die 'sha256sum or shasum is required'
fi
[ "$actual" = "$expected" ] || die 'SHA-256 mismatch; installation cancelled'
tar -xzf "$tmp/$asset" -C "$tmp" fsend
[ -f "$tmp/fsend" ] && [ ! -L "$tmp/fsend" ] || die 'Invalid package'
install_dir=${INSTALL_DIR:-"$HOME/.local/bin"}
mkdir -p "$install_dir"
# Stage on the target filesystem so an existing executable is replaced atomically.
staged=$(mktemp "$install_dir/.fsend-install.XXXXXX")
trap 'rm -f "$staged"; rm -rf "$tmp"' EXIT HUP INT TERM
cp "$tmp/fsend" "$staged"
chmod 755 "$staged"
mv -f "$staged" "$install_dir/fsend"
printf 'Installed %s to %s/fsend\n' "$version" "$install_dir"
case ":$PATH:" in *":$install_dir:"*) ;; *) printf 'Add to PATH: export PATH="%s:$PATH"\n' "$install_dir" ;; esac
