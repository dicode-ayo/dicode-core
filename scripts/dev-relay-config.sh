#!/usr/bin/env bash
# Render a minimal local relay.yaml so buildin/relay-server-body can run without
# Doppler. Stopgap for local/dev use only — the proper dev-mode render path is
# tracked in issue #459. The broker signing key is bootstrapped by the relay
# body itself, so this script does not touch it.
#
# Env overrides:
#   DICODE_DATADIR        data dir (default: $HOME/.dicode)
#   RELAY_PORT            listen port (default: 5553)
#   RELAY_BASE_URL        advertised base URL (default: http://localhost:$RELAY_PORT)
#   RELAY_STATUS_PASSWORD status endpoint password (default: random)
# Pass --force to overwrite an existing relay.yaml.
set -euo pipefail

datadir="${DICODE_DATADIR:-$HOME/.dicode}"
port="${RELAY_PORT:-5553}"
base_url="${RELAY_BASE_URL:-http://localhost:$port}"
dest="$datadir/relay/relay.yaml"

force=0
[ "${1:-}" = "--force" ] && force=1
if [ -e "$dest" ] && [ "$force" -ne 1 ]; then
  echo "dev-relay-config: refusing to overwrite existing $dest (pass --force)" >&2
  exit 1
fi

status_password="${RELAY_STATUS_PASSWORD:-$(openssl rand -hex 16)}"
mkdir -p "$datadir/relay"
umask 077
cat > "$dest" <<YAML
server:
  port: $port
  base_url: $base_url
  tls:
    cert_file: ""
    key_file: ""
status:
  password: $status_password
relay:
  timestamp_tolerance_s: 30
  ping_interval_ms: 30000
  pong_timeout_ms: 10000
  request_timeout_ms: 30000
  nonce_ttl_ms: 60000
broker:
  session_ttl_ms: 300000
  signing_key_file: $datadir/relay/broker-signing.key
  providers: {}
YAML
chmod 600 "$dest"
echo "dev-relay-config: wrote $dest"
echo "dev-relay-config: status password = $status_password"
