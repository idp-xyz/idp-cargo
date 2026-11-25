#!/usr/bin/env bash
# FIN's MCP server for the IDP TELOS gateway (cargo-mcp, V2.101), run locally against the demo database (the instance
# "demo", port 25435, the tenant eii-test that tools/demo/eii-test.sh seeds), which the server only reads. The script
# 1. makes, if there is none, a local gateway key pair (P-256) and a cursor key; 2. writes the configuration: the gateway
# tenant eii-test is CargoPai's tenant eii-test, and the role finance-analyst has fin:ReadProfit for the legal entity LE;
# 3. starts cargo-mcp on 127.0.0.1:8600/mcp, after stopping the one this script started; 4. prints the endpoint and the
# paths. It never migrates, seeds or writes the database: the instance has to be running (tools/demo/eii-test.sh brings it
# up), and the script stops if it is not.
#
# The keys are for local testing only: whoever holds the private key can sign a caller of any tenant and role. In
# production the gateway's public key comes from the platform's operators (docs/api/fin-mcp.md). Everything is kept under
# $FIN_MCP_DIR (default /home/tops/idp-cargo-tools/fin-mcp), never in the repository; the private key, the cursor key and
# the configuration have mode 600. The script prints no key and no assertion.
#
# FIN_MCP_INSTANCE names another localpg instance to read instead of "demo" (a developer's own, seeded with cmd/cargo-demo);
# the tenant is eii-test in either.
#
# Usage: tools/demo/fin-mcp.sh [up|check|stop]
#   up     (default) steps 1 to 4
#   check  the end-to-end check over HTTP against the running server (fin-mcp-check): tools/list without an assertion, each
#          tool for a member of finance-analyst, and the refused calls
#   stop   stop the server this script started
set -euo pipefail

dir=${FIN_MCP_DIR:-/home/tops/idp-cargo-tools/fin-mcp}
instance=${FIN_MCP_INSTANCE:-demo}
addr=127.0.0.1:8600
endpoint=http://$addr/mcp
root=$(cd "$(dirname "$0")/../.." && pwd)
cd "$root"
mkdir -p "$dir/bin"
chmod 700 "$dir"
umask 077

stop_server() {
  # only the process this script started, by its pid, and only while it is still the server it started
  if [ -f "$dir/mcp.pid" ]; then
    pid=$(cat "$dir/mcp.pid")
    exe=$(readlink "/proc/$pid/exe" 2>/dev/null || true)
    if [ "${exe% (deleted)}" = "$dir/bin/cargo-mcp" ]; then
      kill "$pid"
      tail --pid="$pid" -f /dev/null
    fi
    rm -f "$dir/mcp.pid"
  fi
}

action=${1:-up}
case "$action" in
  stop) stop_server; echo "fin-mcp: stopped"; exit 0 ;;
  check)
    go build -o "$dir/bin/fin-mcp-check" ./tools/demo/fin-mcp-check
    exec "$dir/bin/fin-mcp-check" -endpoint "$endpoint" -key "$dir/gateway.key" -gateway-id local
    ;;
  up) ;;
  *) echo "usage: $0 [up|check|stop]" >&2; exit 2 ;;
esac

# 1. the local gateway key pair and the cursor key
if [ ! -s "$dir/gateway.key" ]; then
  openssl ecparam -name prime256v1 -genkey -noout | openssl pkcs8 -topk8 -nocrypt > "$dir/gateway.key"
fi
chmod 600 "$dir/gateway.key"
openssl ec -in "$dir/gateway.key" -pubout > "$dir/gateway.pub.pem" 2>/dev/null
[ -s "$dir/cursor.key" ] || head -c 32 /dev/urandom | base64 -w0 > "$dir/cursor.key"
chmod 600 "$dir/cursor.key"

# 2. the configuration
cat > "$dir/mcp.json" <<JSON
{
  "audience": "$endpoint",
  "gateway_id": "local",
  "public_key_file": "gateway.pub.pem",
  "cursor_key_file": "cursor.key",
  "tenants": {
    "eii-test": {
      "cargo_tenant": "eii-test",
      "roles": {
        "finance-analyst": {"permissions": ["fin:ReadProfit"], "legal_entities": ["LE"]}
      }
    }
  },
  "limits": {"calls_per_minute": 60, "concurrent": 4}
}
JSON
chmod 600 "$dir/mcp.json"

# 3. the server, against the demo database
if ! docker ps --format '{{.Names}}' | grep -qx "idp-cargo-pg16-$instance"; then
  echo "fin-mcp: the database $instance is not running (for demo, tools/demo/eii-test.sh brings it up)" >&2
  exit 1
fi
eval "$(tools/localpg/localpg.sh env "$instance")"
unset IDP_CARGO_MIGRATOR_DSN
go build -o "$dir/bin/cargo-mcp" ./cmd/cargo-mcp
stop_server
nohup "$dir/bin/cargo-mcp" -config "$dir/mcp.json" -addr $addr > "$dir/mcp.log" 2>&1 < /dev/null &
echo $! > "$dir/mcp.pid"
# wait until the server answers the catalogue, which needs no assertion
if ! curl -fsS -o /dev/null --retry 30 --retry-delay 1 --retry-connrefused -X POST "$endpoint" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' -H 'Mcp-Protocol-Version: 2026-07-28' -H 'Mcp-Method: tools/list' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}'; then
  echo "fin-mcp: cargo-mcp did not answer; see $dir/mcp.log" >&2
  exit 1
fi

# 4. where it is
echo "fin-mcp: serving $endpoint (pid $(cat "$dir/mcp.pid"))"
echo "  configuration   $dir/mcp.json"
echo "  gateway key     $dir/gateway.key (private, for local tests only), $dir/gateway.pub.pem (public)"
echo "  cursor key      $dir/cursor.key"
echo "  log             $dir/mcp.log"
echo "  check           tools/demo/fin-mcp.sh check"
