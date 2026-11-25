#!/usr/bin/env bash
# mcp-stale.sh "<JAVA_TOOL_OPTIONS>": check what the MCP JVM does with a stale
# AppCDS archive. It copies the image's jar out, changes its timestamp, mounts
# it over the original and runs mcp-run.sh with the archive mcp-cds.sh built;
# then prints the JVM's [cds] warnings. Expect "timestamp has changed" and the
# default startup and RSS numbers. Source env.sh first.
set -euo pipefail
: "${PERF_DIR:?source scripts/perf/env.sh first}"
# Re-check the isolation here, not only in env.sh: never touch a real install.
case "${SONARLESS_INSTANCE:-}" in
*perftest*) ;;
*)
  echo "$(basename "$0"): SONARLESS_INSTANCE must contain perftest; source scripts/perf/env.sh first" >&2
  exit 1
  ;;
esac
case "${SONARLESS_HOME:-}" in
"$PERF_DIR"/*) ;;
*)
  echo "$(basename "$0"): SONARLESS_HOME must be under PERF_DIR; source scripts/perf/env.sh first" >&2
  exit 1
  ;;
esac
opts=$1
here=$(cd "$(dirname "$0")" && pwd)
image="${SONARLESS_MCP_IMAGE:-sonarsource/sonarqube-mcp:latest}"
[ -f "$PERF_DIR/cds/app.jsa" ] || {
  echo "mcp-stale.sh: no $PERF_DIR/cds/app.jsa; run mcp-cds.sh first" >&2
  exit 1
}
src="$SONARLESS_INSTANCE-jarsrc"
docker rm -f "$src" >/dev/null 2>&1 || true
docker create --name "$src" "$image" >/dev/null
docker cp "$src:/app/sonarqube-mcp-server.jar" "$PERF_DIR/stale.jar"
docker rm "$src" >/dev/null
touch -d '2026-01-01' "$PERF_DIR/stale.jar"
chmod 644 "$PERF_DIR/stale.jar"
"$here/mcp-run.sh" stale "$opts -XX:SharedArchiveFile=/cds/app.jsa" \
  -v "$PERF_DIR/cds:/cds:ro" -v "$PERF_DIR/stale.jar:/app/sonarqube-mcp-server.jar:ro"
grep -F '[cds]' "$PERF_DIR/logs/mcp-stale/container.log" | sort -u || true
rm -f "$PERF_DIR/stale.jar"
