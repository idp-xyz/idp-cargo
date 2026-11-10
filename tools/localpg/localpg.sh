#!/usr/bin/env bash
# Local PostgreSQL 16 for IDP-Cargo integration tests, on ports reserved for IDP-Cargo.
#
# Ports 25430-25439 belong to IDP-Cargo on developer machines. They lie below the default ephemeral ranges
# of Linux (32768-60999) and Windows (49152-65535), so outgoing connections and other tools seldom take them:
#   25432  dev      integration runs (go test -tags integration ./...)
#   25433  upgrade  upgrade runs: migrate and fill with an older commit, then upgrade
#   25434  empty    empty-database migration checks
#   25435-25439     reserved
# Each port is served only by its own container, idp-cargo-pg16-<role>, labelled org.idp-cargo.localpg=<role>.
# The script refuses to start, print a DSN or stop anything when the port is held by something else, so tests
# and migrations never reach another program's server.
#
# Usage: tools/localpg/localpg.sh up|env|grants|down <dev|upgrade|empty>
#   up      start the container (postgres:16.15) unless it runs already, and wait until it accepts connections
#   env     print export lines for IDP_CARGO_MIGRATOR_DSN and IDP_CARGO_POSTGRES_DSN, as CI sets them
#   grants  create the application role and apply the grants of the CI workflow (run after the migrations)
#   down    remove the container and its data
set -euo pipefail

image=postgres:16.15
action=${1:-}
role=${2:-}
case "$role" in
  dev) port=25432 ;;
  upgrade) port=25433 ;;
  empty) port=25434 ;;
  *) echo "usage: $0 up|env|grants|down <dev|upgrade|empty>" >&2; exit 2 ;;
esac
name=idp-cargo-pg16-$role
label=org.idp-cargo.localpg
root=$(cd "$(dirname "$0")/../.." && pwd)

ours() { [ "$(docker inspect -f "{{index .Config.Labels \"$label\"}}" "$name" 2>/dev/null)" = "$role" ]; }
running() { [ "$(docker inspect -f '{{.State.Running}}' "$name" 2>/dev/null)" = true ]; }
listening() { (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; }
mapped() { docker port "$name" 5432/tcp 2>/dev/null | grep -qx "127.0.0.1:$port"; }

# guard succeeds only when the port is served by this role's own running container.
guard() {
  if ours && running && mapped; then return 0; fi
  if listening; then
    echo "localpg: 127.0.0.1:$port is held by something other than $name; refusing" >&2
  else
    echo "localpg: $name is not running; run: $0 up $role" >&2
  fi
  exit 1
}

case "$action" in
  up)
    if ours && running && mapped; then echo "localpg: $name runs on 127.0.0.1:$port"; exit 0; fi
    if docker inspect "$name" >/dev/null 2>&1 && ! ours; then
      echo "localpg: a container named $name exists without the $label label; refusing" >&2; exit 1
    fi
    if listening; then echo "localpg: 127.0.0.1:$port is held by another program; refusing" >&2; exit 1; fi
    docker rm -f "$name" >/dev/null 2>&1 || true
    docker run -d --name "$name" --label "$label=$role" -e POSTGRES_PASSWORD=ci-only -e POSTGRES_DB=cargo_contract \
      -p "127.0.0.1:$port:5432" "$image" >/dev/null
    for _ in $(seq 1 60); do
      if docker exec "$name" pg_isready -U postgres -d cargo_contract -h 127.0.0.1 >/dev/null 2>&1; then
        echo "localpg: $name runs on 127.0.0.1:$port"; exit 0
      fi
      sleep 1
    done
    echo "localpg: $name did not become ready" >&2; exit 1 ;;
  env)
    guard
    echo "export IDP_CARGO_MIGRATOR_DSN='postgres://postgres:ci-only@127.0.0.1:$port/cargo_contract?sslmode=disable'"
    echo "export IDP_CARGO_POSTGRES_DSN='postgres://cargo_app:ci-app-only@127.0.0.1:$port/cargo_contract?sslmode=disable'" ;;
  grants)
    guard
    # The SQL of the workflow step that isolates the application role is the one source of the grants.
    sql=$(awk '/Isolate application from migrator/{f=1} f&&/<<.SQL.$/{s=1;next} s&&/^ *SQL$/{exit} s{sub(/^          /,"");print}' \
      "$root/.github/workflows/org-pg16-contract.yml")
    [ -n "$sql" ] || { echo "localpg: no grants found in the workflow" >&2; exit 1; }
    if docker exec "$name" psql -U postgres -d cargo_contract -tAc "SELECT 1 FROM pg_roles WHERE rolname='cargo_app'" | grep -q 1; then
      sql=$(printf '%s\n' "$sql" | grep -v '^CREATE ROLE cargo_app ')
    fi
    printf '%s\n' "$sql" | docker exec -i "$name" psql -U postgres -d cargo_contract -v ON_ERROR_STOP=1 -q
    echo "localpg: grants applied to $name" ;;
  down)
    if ! docker inspect "$name" >/dev/null 2>&1; then echo "localpg: no $name"; exit 0; fi
    ours || { echo "localpg: $name lacks the $label label; refusing" >&2; exit 1; }
    docker rm -f "$name" >/dev/null
    echo "localpg: $name removed" ;;
  *) echo "usage: $0 up|env|grants|down <dev|upgrade|empty>" >&2; exit 2 ;;
esac
