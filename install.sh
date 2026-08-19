#!/bin/sh
set -eu

repository=ahodges22/mcpd
install_dir=${MCPD_INSTALL_DIR:-"$HOME/.local/bin"}

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *)
    printf 'mcpd does not support this operating system.\n' >&2
    exit 1
    ;;
esac

case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *)
    printf 'mcpd does not support this CPU architecture.\n' >&2
    exit 1
    ;;
esac

if [ -n "${MCPD_VERSION:-}" ]; then
  case "$MCPD_VERSION" in
    v*) version=$MCPD_VERSION ;;
    *) version=v$MCPD_VERSION ;;
  esac
else
  latest=$(curl --proto '=https' --tlsv1.2 -fsSLI -o /dev/null -w '%{url_effective}' \
    "https://github.com/$repository/releases/latest")
  version=${latest##*/}
fi
case "$version" in
  v[0-9]*) ;;
  *)
    printf 'Could not resolve a valid mcpd release version: %s\n' "$version" >&2
    exit 1
    ;;
esac

archive=mcpd_${os}_${arch}.tar.gz
release_url=https://github.com/$repository/releases/download/$version
temporary=$(mktemp -d "${TMPDIR:-/tmp}/mcpd-install.XXXXXX")
staged=
cleanup() {
  rm -rf "$temporary"
  if [ -n "$staged" ]; then
    rm -f "$staged"
  fi
}
trap cleanup EXIT HUP INT TERM

for asset in "$archive" checksums.txt checksums.txt.cosign; do
  curl --proto '=https' --tlsv1.2 -fsSL "$release_url/$asset" -o "$temporary/$asset"
done

if command -v cosign >/dev/null 2>&1; then
  cosign verify-blob \
    --bundle "$temporary/checksums.txt.cosign" \
    --certificate-identity "https://github.com/$repository/.github/workflows/release.yml@refs/tags/$version" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    "$temporary/checksums.txt" >/dev/null
else
  printf 'warning: cosign is unavailable; verifying the archive checksum without its Sigstore signature\n' >&2
fi

expected=$(awk -v name="$archive" '$2 == name || $2 == "*" name { print $1; exit }' "$temporary/checksums.txt")
if [ -z "$expected" ]; then
  printf 'Release checksums do not contain %s.\n' "$archive" >&2
  exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$temporary/$archive" | awk '{print $1}')
else
  actual=$(shasum -a 256 "$temporary/$archive" | awk '{print $1}')
fi
if [ "$actual" != "$expected" ]; then
  printf 'Checksum verification failed for %s.\n' "$archive" >&2
  exit 1
fi

mkdir "$temporary/extract"
tar -xzf "$temporary/$archive" -C "$temporary/extract" mcpd
mkdir -p "$install_dir"
staged=$(mktemp "$install_dir/.mcpd-install.XXXXXX")
install -m 0755 "$temporary/extract/mcpd" "$staged"
mv -f "$staged" "$install_dir/mcpd"
staged=

printf 'Installed mcpd %s at %s/mcpd.\n' "$version" "$install_dir"
case ":$PATH:" in
  *":$install_dir:"*) ;;
  *) printf 'Add %s to PATH before opening a new shell.\n' "$install_dir" ;;
esac

if [ "${MCPD_SKIP_SETUP:-0}" != 1 ]; then
  if ( : </dev/tty ) 2>/dev/null; then
    "$install_dir/mcpd" setup </dev/tty
  else
    printf 'No interactive terminal is available. Run %s/mcpd setup to finish configuration.\n' "$install_dir"
  fi
fi
