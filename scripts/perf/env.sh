# shellcheck shell=bash
# Source this before the other scripts in scripts/perf. It points every
# sonarless-mcp call at a throwaway instance so a real install is never touched:
# its own SONARLESS_HOME, HOME and XDG dirs under PERF_DIR, the instance name
# perftest (containers perftest-server and perftest-mcp, network perftest,
# volumes perftest-server-<version>-*) and ports away from the defaults.
#
#   export PERF_DIR=/tmp/sl-perf; . scripts/perf/env.sh
#
# Build the binary with `go build -buildvcs=false -o "$PERF_DIR/sonarless-mcp" ./cmd/sonarless-mcp`.

: "${PERF_DIR:?set PERF_DIR to an empty scratch dir}"
: "${SONARLESS_BIN:=$PERF_DIR/sonarless-mcp}"
mkdir -p "$PERF_DIR/home" "$PERF_DIR/xdg/config" "$PERF_DIR/xdg/cache" "$PERF_DIR/xdg/data" "$PERF_DIR/xdg/state" "$PERF_DIR/logs"

export PERF_DIR SONARLESS_BIN
export SONARLESS_INSTANCE="${SONARLESS_INSTANCE:-perftest}"
case "$SONARLESS_INSTANCE" in
*perftest*) ;;
*)
  echo "env.sh: SONARLESS_INSTANCE must contain perftest" >&2
  return 1
  ;;
esac
export SONARLESS_HOME="$PERF_DIR/home" HOME="$PERF_DIR/home"
export XDG_CONFIG_HOME="$PERF_DIR/xdg/config" XDG_CACHE_HOME="$PERF_DIR/xdg/cache"
export XDG_DATA_HOME="$PERF_DIR/xdg/data" XDG_STATE_HOME="$PERF_DIR/xdg/state"
export SONARLESS_AUTO_UPDATE=false SONARLESS_UPDATE_NOTICE=false SONARLESS_IDLE_TIMEOUT=0
export SONARLESS_SERVER_PORT="${SONARLESS_SERVER_PORT:-19234}"
export SONARLESS_MCP_PORT="${SONARLESS_MCP_PORT:-19235}"
export SONARLESS_GATEWAY_PORT="${SONARLESS_GATEWAY_PORT:-19233}"
export PERF_MCPX_PORT="${PERF_MCPX_PORT:-19236}"
unset SONARLESS_GATEWAY_TOKEN

sl() { "$SONARLESS_BIN" "$@"; }
