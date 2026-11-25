#!/usr/bin/env bash
# mcp-nmt.sh <label> "<JAVA_TOOL_OPTIONS>" [extra docker run args...]: run
# mcp-run.sh with -XX:NativeMemoryTracking=summary added and, after the agent
# session, read `jcmd <pid> VM.native_memory summary` into
# $PERF_DIR/logs/mcp-<label>/hook.txt. The MCP image ships only `java`, so jcmd
# runs from a JDK 21 sidecar (PERF_JDK_IMAGE) that shares the MCP container's
# PID namespace and user (appuser, uid 100). Source env.sh first.
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
label=$1 opts=$2
shift 2
here=$(cd "$(dirname "$0")" && pwd)
jdk="${PERF_JDK_IMAGE:-maven:3-eclipse-temurin-21}"
export HOOK="docker run --rm --name $SONARLESS_INSTANCE-jcmd --pid container:$SONARLESS_INSTANCE-mcpx --user 100:101 -e HOME=/tmp $jdk sh -c 'jcmd \$(pgrep -f sonarqube-mcp-server.jar | head -1) VM.native_memory summary'"
"$here/mcp-run.sh" "$label" "$opts -XX:NativeMemoryTracking=summary" "$@"
grep -E '^Total|^-' "$PERF_DIR/logs/mcp-$label/hook.txt"
