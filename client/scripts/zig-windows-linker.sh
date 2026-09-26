#!/usr/bin/env bash
# Linker shim so `cargo build --target x86_64-pc-windows-gnu` works on Linux with only zig installed.
# Drops gcc-runtime flags rustc passes for MinGW that zig's bundled toolchain provides itself.
args=()
for a in "$@"; do
  case "$a" in
    -lgcc_eh) args+=("-lunwind") ;;  # zig's libunwind replaces libgcc's unwinder
    -lgcc|-lgcc_s|-l:libpthread.a|-lwinpthread|-lmsvcrt) ;;
    # zig treats -Bdynamic as "only .dll", which hides MinGW import libraries (.a).
    -Wl,-Bdynamic|-Wl,-Bstatic) ;;
    --target=*|-Wl,--disable-auto-image-base|-Wl,--dynamicbase) ;;
    *) args+=("$a") ;;
  esac
done
exec zig cc -target x86_64-windows-gnu "${args[@]}"
