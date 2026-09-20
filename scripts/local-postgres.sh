#!/usr/bin/env bash
set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
postgres_bin="$repository_root/.tools/postgres/usr/lib/postgresql/16/bin"
database_dir="$repository_root/data/postgres"
socket_dir="$repository_root/.tools/postgres-socket"
log_file="$repository_root/.tools/postgres.log"
port="${TRAJECTORY_POSTGRES_PORT:-55432}"

if [[ ! -x "$postgres_bin/postgres" ]]; then
  echo "Local PostgreSQL is missing from .tools/postgres." >&2
  exit 127
fi

mkdir -p "$socket_dir"

case "${1:-}" in
  start)
    if [[ ! -f "$database_dir/PG_VERSION" ]]; then
      mkdir -p "$database_dir"
      "$postgres_bin/initdb" -D "$database_dir" --username=trajectory --auth=trust --encoding=UTF8 --no-locale
    fi
    if "$postgres_bin/pg_ctl" -D "$database_dir" status >/dev/null 2>&1; then
      echo "PostgreSQL is already running on port $port."
      exit 0
    fi
    "$postgres_bin/pg_ctl" -D "$database_dir" -l "$log_file" -o "-h 127.0.0.1 -p $port -k $socket_dir" start
    if ! "$postgres_bin/psql" -h 127.0.0.1 -p "$port" -U trajectory -d postgres -Atqc "SELECT 1 FROM pg_database WHERE datname='trajectory'" | grep -qx 1; then
      "$postgres_bin/createdb" -h 127.0.0.1 -p "$port" -U trajectory trajectory
    fi
    echo "PostgreSQL ready: postgres://trajectory@127.0.0.1:$port/trajectory?sslmode=disable"
    ;;
  stop)
    if "$postgres_bin/pg_ctl" -D "$database_dir" status >/dev/null 2>&1; then
      "$postgres_bin/pg_ctl" -D "$database_dir" stop -m fast
    else
      echo "PostgreSQL is not running."
    fi
    ;;
  status)
    "$postgres_bin/pg_ctl" -D "$database_dir" status
    ;;
  psql)
    shift
    exec "$postgres_bin/psql" -h 127.0.0.1 -p "$port" -U trajectory -d trajectory "$@"
    ;;
  *)
    echo "Usage: scripts/local-postgres.sh {start|stop|status|psql}" >&2
    exit 2
    ;;
esac

