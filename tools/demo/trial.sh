#!/usr/bin/env bash
# A place to try the API by hand (stage 3, the owner's decision of 2026-11-26): the tenant "trial" on the local
# PostgreSQL 16 instance "trial" (port 25438). The script 1. brings the instance up, migrates it and grants the
# application role; 2. writes a token key if there is none and mints a development token for each job role
# (cargo-api -mint-token -role: ops, warehouse, docs, finance, finance-manager, privacy, admin, consultant), each for a
# person of its own; 3. starts cargo-api -dev-tokens on 127.0.0.1:8097 against the instance, after stopping the one this
# script started; 4. registers and verifies the forwarder's legal entity LE (the same commands on every run, so a second
# run changes nothing). Open http://127.0.0.1:8097/docs, press "Authorize" and paste a role's token: every route is
# there to read and try.
#
# The key, the tokens, the binaries and the server's pid and log are kept under $TRIAL_DIR (default
# /home/tops/idp-cargo-tools/trial), never in the repository; files with secrets have mode 600. The script prints the
# URLs and the paths, never the key or a token. Tokens live 24 hours: run "tokens" for new ones.
#
# Usage: tools/demo/trial.sh [up|tokens|stop]
set -euo pipefail

dir=${TRIAL_DIR:-/home/tops/idp-cargo-tools/trial}
addr=127.0.0.1:8097
tenant=trial
roles="ops warehouse docs finance finance-manager privacy admin consultant"
root=$(cd "$(dirname "$0")/../.." && pwd)
cd "$root"
mkdir -p "$dir/bin" "$dir/tokens"
chmod 700 "$dir"
umask 077

stop_server() {
  # only the process this script started, by its pid, and only while it is still the server it started
  if [ -f "$dir/api.pid" ]; then
    pid=$(cat "$dir/api.pid")
    exe=$(readlink "/proc/$pid/exe" 2>/dev/null || true)
    if [ "${exe% (deleted)}" = "$dir/bin/cargo-api" ]; then
      kill "$pid"
      for _ in $(seq 1 50); do kill -0 "$pid" 2>/dev/null || break; sleep 0.2; done
    fi
    rm -f "$dir/api.pid"
  fi
}

mint_tokens() {
  if [ ! -s "$dir/token.key" ]; then
    head -c 32 /dev/urandom | base64 -w0 > "$dir/token.key"
  fi
  chmod 600 "$dir/token.key"
  export IDP_CARGO_API_TOKEN_KEY=$(cat "$dir/token.key")
  for role in $roles; do
    "$dir/bin/cargo-api" -mint-token -tenant $tenant -actor "$role-1" -role "$role" -ttl 24h > "$dir/tokens/$role.token"
  done
  chmod 600 "$dir"/tokens/*.token
}

action=${1:-up}
case "$action" in
  stop) stop_server; echo "trial: stopped"; exit 0 ;;
esac

go build -o "$dir/bin/cargo-api" ./cmd/cargo-api
go build -o "$dir/bin/cargo-migrate" ./cmd/cargo-migrate

if [ "$action" = tokens ]; then mint_tokens; echo "trial: tokens in $dir/tokens/<role>.token"; exit 0; fi

tools/localpg/localpg.sh up trial
eval "$(tools/localpg/localpg.sh env trial)"
"$dir/bin/cargo-migrate" -manifest migrations/manifest/contexts.txt -apply | tail -n 1
tools/localpg/localpg.sh grants trial

mint_tokens
stop_server
IDP_CARGO_API_TOKEN_KEY=$(cat "$dir/token.key") nohup "$dir/bin/cargo-api" -dev-tokens -addr $addr > "$dir/api.log" 2>&1 < /dev/null &
echo $! > "$dir/api.pid"
for _ in $(seq 1 50); do
  curl -fsS "http://$addr/readyz" >/dev/null 2>&1 && break
  kill -0 "$(cat "$dir/api.pid")" 2>/dev/null || { echo "trial: cargo-api stopped; see $dir/api.log" >&2; exit 1; }
  sleep 0.2
done

# the forwarder's legal entity, the same commands on every run (a second run replays them)
[ -s "$dir/seed-at" ] || date -u -d '1 hour ago' +%Y-%m-%dT%H:00:00Z > "$dir/seed-at"
at=$(cat "$dir/seed-at")
admin=$(cat "$dir/tokens/admin.token")
call() {
  curl -fsS -o /dev/null -X POST "http://$addr$1" -H "Authorization: Bearer $admin" -H "Content-Type: application/json" \
    -H "Idempotency-Key: $2" ${3:+-H "If-Match: $3"} --data "$4" < /dev/null
}
call /v1/legal-entities trial-le-1 "" '{"legal_entity_id":"LE","registered_name":"Trial Forwarding Co., Ltd.","jurisdiction":"CN","evidence_ref":"business licence","occurred_at":"'"$at"'"}'
call /v1/legal-entities/LE/verify trial-le-2 '"1"' '{"occurred_at":"'"$at"'"}' || true

echo "trial: the API's page http://$addr/docs (tenant $tenant, development tokens), its document http://$addr/docs/openapi.json"
echo "trial: a token for each role in $dir/tokens/<role>.token ($roles); server pid $(cat "$dir/api.pid"), log $dir/api.log"
