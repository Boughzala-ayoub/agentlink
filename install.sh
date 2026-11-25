#!/bin/sh
# agentlink installer for macOS and Linux. Run it with:
#   curl -fsSL https://raw.githubusercontent.com/Boughzala-ayoub/agentlink/master/install.sh | sh
# It downloads one executable, checks its SHA-256 against the release's
# checksums.txt, puts it in ~/.agentlink/bin and runs `agentlink setup`, which
# shows what it will change and asks once. Nothing else keeps running.
set -eu
repo="${AGENTLINK_REPO:-Boughzala-ayoub/agentlink}"
base="${AGENTLINK_BASE_URL:-https://github.com/$repo/releases/latest/download}"
case "$(uname -s)" in Darwin) os=darwin ;; Linux) os=linux ;; *) echo "agentlink: unsupported OS $(uname -s)" >&2; exit 1 ;; esac
case "$(uname -m)" in x86_64 | amd64) arch=amd64 ;; arm64 | aarch64) arch=arm64 ;; *) echo "agentlink: unsupported CPU $(uname -m)" >&2; exit 1 ;; esac
asset="agentlink-$os-$arch"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
echo "Downloading $asset ..."
curl -fsSL "$base/$asset" -o "$tmp/agentlink"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"
want="$(awk -v a="$asset" '$2 == a || $2 == "*" a { print $1; exit }' "$tmp/checksums.txt")"
[ -n "$want" ] || { echo "agentlink: checksums.txt has no line for $asset. Nothing was installed." >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then got="$(sha256sum "$tmp/agentlink" | cut -d' ' -f1)"; else got="$(shasum -a 256 "$tmp/agentlink" | cut -d' ' -f1)"; fi
[ "$got" = "$want" ] || { echo "agentlink: checksum mismatch for $asset: got $got, expected $want. Nothing was installed." >&2; exit 1; }
bin="${AGENTLINK_HOME:-$HOME/.agentlink}/bin"
mkdir -p "$bin"
chmod 755 "$tmp/agentlink"
mv -f "$tmp/agentlink" "$bin/agentlink" # running copies keep their old inode
# The script itself arrives on stdin, so setup asks its question on the terminal.
if [ -r /dev/tty ] && (: </dev/tty) 2>/dev/null; then
  "$bin/agentlink" setup </dev/tty
else
  echo "agentlink is in $bin. No terminal to ask on, so run this next: $bin/agentlink setup"
fi
