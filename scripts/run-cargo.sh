#!/usr/bin/env bash
set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export CARGO_HOME="$repository_root/.tools/cargo"
export RUSTUP_HOME="$repository_root/.tools/rustup"

if [[ ! -x "$CARGO_HOME/bin/cargo" ]]; then
  echo "Rust is required. Install it under .tools/cargo and .tools/rustup." >&2
  exit 127
fi

if ! command -v cc >/dev/null 2>&1 && [[ -x "$repository_root/.tools/gcc/usr/bin/x86_64-linux-gnu-gcc-13" ]]; then
  gcc_root="$repository_root/.tools/gcc"
  export PATH="$gcc_root/usr/bin:$PATH"
  export LD_LIBRARY_PATH="$gcc_root/usr/lib/x86_64-linux-gnu${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
  export CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_LINKER="$gcc_root/usr/bin/x86_64-linux-gnu-gcc-13"
  if [[ -x "$gcc_root/usr/bin/x86_64-w64-mingw32-gcc-posix" ]]; then
    export CARGO_TARGET_X86_64_PC_WINDOWS_GNU_LINKER="$gcc_root/usr/bin/x86_64-w64-mingw32-gcc-posix"
  fi
fi

exec "$CARGO_HOME/bin/cargo" "$@"
