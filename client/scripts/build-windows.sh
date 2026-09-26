#!/usr/bin/env bash
# Cross-compiles the Windows client on Linux using zig as linker/dlltool (no MinGW needed).
# Needs: rustup target add x86_64-pc-windows-gnu, and zig on PATH.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
cd "$here/.."
PATH="$here:$PATH" \
CARGO_TARGET_X86_64_PC_WINDOWS_GNU_LINKER="$here/zig-windows-linker.sh" \
  cargo build --release --target x86_64-pc-windows-gnu "$@"
echo "Built: $(pwd)/target/x86_64-pc-windows-gnu/release/attention-getter.exe"
