#!/usr/bin/env bash
set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if command -v go >/dev/null 2>&1; then
  go_binary="$(command -v go)"
elif [[ -x "$repository_root/.tools/go/bin/go" ]]; then
  go_binary="$repository_root/.tools/go/bin/go"
else
  echo "Go is required. Install Go or place it at .tools/go/bin/go." >&2
  exit 127
fi

cd "$repository_root/services/api"
exec "$go_binary" "$@"

