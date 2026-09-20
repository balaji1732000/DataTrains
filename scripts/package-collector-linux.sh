#!/usr/bin/env bash
set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bundle="${1:-deb}"

if [[ "$bundle" != "deb" && "$bundle" != "appimage" ]]; then
  echo "Usage: scripts/package-collector-linux.sh [deb|appimage]" >&2
  exit 2
fi
if [[ "$(uname -s)" != "Linux" ]]; then
  echo "Linux collector packages must be built on Linux." >&2
  exit 2
fi

cargo_root="$repository_root/.tools/cargo"
rustup_root="$repository_root/.tools/rustup"
gcc_root="$repository_root/.tools/gcc"
dependency_root="$repository_root/.tools/linux-deps/root"

if ! command -v cargo >/dev/null 2>&1 && [[ -x "$cargo_root/bin/cargo" ]]; then
  export CARGO_HOME="$cargo_root"
  export RUSTUP_HOME="$rustup_root"
  export PATH="$cargo_root/bin:$PATH"
fi

if ! command -v cc >/dev/null 2>&1 && [[ -x "$gcc_root/usr/bin/x86_64-linux-gnu-gcc-13" ]]; then
  export PATH="$gcc_root/usr/bin:$PATH"
  export CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_LINKER="$gcc_root/usr/bin/x86_64-linux-gnu-gcc-13"
  export CC="$gcc_root/usr/bin/x86_64-linux-gnu-gcc-13"
  export LD_LIBRARY_PATH="$gcc_root/usr/lib/x86_64-linux-gnu${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
fi

if ! command -v pkg-config >/dev/null 2>&1 && [[ -x "$dependency_root/usr/bin/pkgconf" ]]; then
  export PATH="$dependency_root/usr/bin:$PATH"
  export PKG_CONFIG="$dependency_root/usr/bin/pkgconf"
  export PKG_CONFIG_PATH="$dependency_root/usr/lib/x86_64-linux-gnu/pkgconfig:$dependency_root/usr/lib/pkgconfig:$dependency_root/usr/share/pkgconfig"
  export PKG_CONFIG_SYSROOT_DIR="$dependency_root"
  export CFLAGS="-I$dependency_root/usr/include/x86_64-linux-gnu${CFLAGS:+ $CFLAGS}"
  export LD_LIBRARY_PATH="$dependency_root/usr/lib/x86_64-linux-gnu${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"

  # linuxdeploy's GTK plugin also copies runtime input-method and image-loader
  # modules. The extracted development packages do not contain those modules,
  # so stage the matching host copies in the ignored local dependency root.
  if [[ "$bundle" == "appimage" ]]; then
    for module_directory in gtk-3.0 gdk-pixbuf-2.0; do
      bundled_modules="$dependency_root/usr/lib/x86_64-linux-gnu/$module_directory"
      host_modules="/usr/lib/x86_64-linux-gnu/$module_directory"
      if [[ ! -d "$bundled_modules" && -d "$host_modules" ]]; then
        mkdir -p "$(dirname "$bundled_modules")"
        cp -a "$host_modules" "$bundled_modules"
      fi
    done
  fi
fi

if ! command -v cargo >/dev/null 2>&1; then
  echo "Rust/Cargo is required to package the Linux collector." >&2
  exit 2
fi
if ! command -v pkg-config >/dev/null 2>&1 && [[ -z "${PKG_CONFIG:-}" ]]; then
  echo "pkg-config and the GTK/WebKit development packages are required." >&2
  exit 2
fi

cd "$repository_root"
exec npm run tauri --workspace=@trajectory/collector -- build --bundles "$bundle"
