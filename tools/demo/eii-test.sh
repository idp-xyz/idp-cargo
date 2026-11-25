#!/usr/bin/env bash
# The test tenant eii-test for EII (the other IDP project that reads FIN's profit and asks for cost reviews), on the
# local PostgreSQL 16 instance "demo" (port 25435). The script 1. brings the instance up, migrates it and grants the
# application role; 2. seeds the tenant with cmd/cargo-demo, through the contexts' commands (a second run changes
# nothing); 3. writes a token key if there is none and mints development tokens with cargo-api -mint-token: alice       
# fin:ReadProfit, fin:RequestCostReview fin-reviewer   fin:ReadProfit, fin:ResolveCostReview 4. starts cargo-api
# -dev-tokens on 127.0.0.1:8095 against the instance, after stopping the one this script started. The key, the tokens,
# the seed's start time, the binaries and the server's pid and log are kept under $EII_TEST_DIR (default
# /home/tops/idp-cargo-tools/eii-test), never in the repository; files with secrets have mode 600. The script prints the
# base URL and the paths, never the key or a token.
#
# Tokens are development tokens, HS256-signed with the key: whoever holds the key can sign any tenant, actor and
# permission. The verifier accepts a token for 24 hours at most, so run the script again (or "tokens") to mint new ones.
#
# Usage: tools/demo/eii-test.sh [up|tokens|stop]
set -euo pipefail

dir=${EII_TEST_DIR:-/home/tops/idp-cargo-tools/eii-test}
addr=127.0.0.1:8095
tenant=eii-test
root=$(cd "$(dirname "$0")/../.." && pwd)
cd "$root"
mkdir -p "$dir/bin"
chmod 700 "$dir"
umask 077

stop_server() {
  # only the process this script started, by its pid, and only while it is still the server it started
  if [ -f "$dir/api.pid" ]; then
    pid=$(cat "$dir/api.pid")
    # a binary built over a running one reads "(deleted)" in /proc
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
  "$dir/bin/cargo-api" -mint-token -tenant $tenant -actor alice -permissions fin:ReadProfit,fin:RequestCostReview -ttl 24h > "$dir/alice.token"
  "$dir/bin/cargo-api" -mint-token -tenant $tenant -actor fin-reviewer -permissions fin:ReadProfit,fin:ResolveCostReview -ttl 24h > "$dir/fin-reviewer.token"
  chmod 600 "$dir/alice.token" "$dir/fin-reviewer.token"
}

action=${1:-up}
case "$action" in
  stop) stop_server; echo "eii-test: stopped"; exit 0 ;;
esac

go build -o "$dir/bin/cargo-api" ./cmd/cargo-api
go build -o "$dir/bin/cargo-demo" ./cmd/cargo-demo
go build -o "$dir/bin/cargo-migrate" ./cmd/cargo-migrate

if [ "$action" = tokens ]; then mint_tokens; echo "eii-test: tokens in $dir (alice.token, fin-reviewer.token)"; exit 0; fi

tools/localpg/localpg.sh up demo
eval "$(tools/localpg/localpg.sh env demo)"
"$dir/bin/cargo-migrate" -manifest migrations/manifest/contexts.txt -apply | tail -n 1
tools/localpg/localpg.sh grants demo

# the seed's day: fixed on the first run, so that a run later sends the same commands
[ -s "$dir/seed-at" ] || date -u -d '48 hours ago' +%Y-%m-%dT%H:00:00Z > "$dir/seed-at"
"$dir/bin/cargo-demo" -tenant $tenant -at "$(cat "$dir/seed-at")"

mint_tokens
stop_server
IDP_CARGO_API_TOKEN_KEY=$(cat "$dir/token.key") nohup "$dir/bin/cargo-api" -dev-tokens -addr $addr > "$dir/api.log" 2>&1 < /dev/null &
echo $! > "$dir/api.pid"
for _ in $(seq 1 50); do
  curl -fsS "http://$addr/readyz" >/dev/null 2>&1 && break
  kill -0 "$(cat "$dir/api.pid")" 2>/dev/null || { echo "eii-test: cargo-api stopped; see $dir/api.log" >&2; exit 1; }
  sleep 0.2
done
echo "eii-test: base URL http://$addr/v1/fin/ (development tokens, tenant $tenant)"
echo "eii-test: tokens $dir/alice.token and $dir/fin-reviewer.token; server pid $(cat "$dir/api.pid"), log $dir/api.log"
