#!/bin/sh
# Installs or updates the Attention Getter client on Linux (x86_64).
#
#   curl -fsSL https://raw.githubusercontent.com/pakkid/attention-getter-v2/main/client/scripts/install.sh | sh
#
# Downloads the latest release (or $AG_VERSION, e.g. "v0.1.1"), checks it against
# SHA256SUMS, replaces any existing install, runs `setup` if this PC isn't paired yet, and
# enables start at login (systemd user unit, plus the GNOME extension on GNOME).
# Re-run it any time to update; the pairing and settings are kept.
set -eu

repo=pakkid/attention-getter-v2
asset=attention-getter-linux-x86_64
if [ -n "${AG_VERSION:-}" ]; then
    base="https://github.com/$repo/releases/download/$AG_VERSION"
else
    base="https://github.com/$repo/releases/latest/download"
fi
bindir="$HOME/.local/bin"
exe="$bindir/attention-getter"
config="${XDG_CONFIG_HOME:-$HOME/.config}/attention-getter/config.toml"
unit="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/attention-getter.service"
ext="${XDG_DATA_HOME:-$HOME/.local/share}/gnome-shell/extensions/attention-getter@pakkid/extension.js"

[ "$(uname -s)" = Linux ] && [ "$(uname -m)" = x86_64 ] || {
    echo "Only Linux x86_64 has a prebuilt client; build from source instead." >&2
    exit 1
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "Downloading $base/$asset"
curl -fsSL "$base/$asset" -o "$tmp/$asset"
curl -fsSL "$base/SHA256SUMS" -o "$tmp/SHA256SUMS"
(cd "$tmp" && grep " \*\{0,1\}$asset\$" SHA256SUMS | sha256sum -c --quiet -) || {
    echo "Checksum mismatch; not installing." >&2
    exit 1
}
echo "Checksum OK"
chmod +x "$tmp/$asset"

# Replace an existing install wherever it lives: the unit's ExecStart names the exe.
old=""
if [ -f "$unit" ]; then
    old=$(sed -n 's/^ExecStart="\{0,1\}\([^"]*\)"\{0,1\} run$/\1/p' "$unit" | head -n 1)
fi
if systemctl --user is-active --quiet attention-getter.service 2>/dev/null; then
    echo "Stopping the running client"
    systemctl --user stop attention-getter.service
fi
mkdir -p "$bindir"
mv -f "$tmp/$asset" "$exe"
if [ -n "$old" ] && [ "$old" != "$exe" ] && [ -f "$old" ]; then
    rm -f "$old"
    echo "Removed the previous copy at $old"
fi
echo "Installed $exe"

if [ ! -f "$config" ]; then
    # stdin is this script when piped from curl; setup needs the terminal.
    if (: </dev/tty) 2>/dev/null; then
        "$exe" setup </dev/tty
    else
        echo "Not paired yet and no terminal for setup. Run '$exe setup', then run this installer again." >&2
        exit 1
    fi
fi

ext_before=$(cat "$ext" 2>/dev/null | sha256sum)
"$exe" install
if [ -f "$ext" ] && [ "$ext_before" != "$(sha256sum <"$ext")" ] && [ -n "$old" ]; then
    echo "The GNOME extension changed: log out and back in to load the new version."
fi

case ":$PATH:" in
*":$bindir:"*) ;;
*) echo "Note: $bindir is not on your PATH." ;;
esac
echo "Done. Preview the popup with: attention-getter test"
