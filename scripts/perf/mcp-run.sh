#!/usr/bin/env bash
# mcp-run.sh <label> "<JAVA_TOOL_OPTIONS>" [extra docker run args...]: start one
# throwaway MCP container (<instance>-mcpx) the way sonarless-mcp starts the
# shared one, on the perftest network and server; print the time from
# `docker run` to /health answering 200, the java RSS and the container memory
# 10 s after ready and after an agent-like session (mcp-session.py), then stop it
# with SIGTERM and keep its log. HOOK, when set, runs (bash -c) after the
# session while the container is up, e.g. a jcmd sidecar. Source env.sh first;
# the perftest server must be running (sl start) for the token and the tools.
set -euo pipefail
: "${PERF_DIR:?source scripts/perf/env.sh first}"
label=$1 opts=$2
shift 2
here=$(cd "$(dirname "$0")" && pwd)
N="$SONARLESS_INSTANCE-mcpx" P=$PERF_MCPX_PORT L="$PERF_DIR/logs/mcp-$label"
mkdir -p "$L"
docker rm -f "$N" >/dev/null 2>&1 || true

t0=$(date +%s%N)
docker run -d --init --name "$N" --network "$SONARLESS_INSTANCE" -p "127.0.0.1:$P:8080" \
  -e SONARQUBE_TRANSPORT=http -e SONARQUBE_HTTP_HOST=0.0.0.0 \
  -e "SONARQUBE_URL=http://$SONARLESS_INSTANCE-server:9000" -e SONARQUBE_LOG_TO_FILE_DISABLED=true \
  -e SONARQUBE_TOOLSETS=coverage,duplications,issues,measures,projects,quality-gates,rules,security-hotspots \
  -e "JAVA_TOOL_OPTIONS=$opts" "$@" "${SONARLESS_MCP_IMAGE:-sonarsource/sonarqube-mcp:latest}" >/dev/null
until [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 1 "http://127.0.0.1:$P/health")" = 200 ]; do
  sleep 0.05
  if [ $((($(date +%s%N) - t0) / 1000000000)) -gt 120 ]; then
    echo "$label: not ready after 120s" >&2
    docker logs "$N" 2>&1 | tail >&2
    exit 1
  fi
done
t1=$(date +%s%N)

rss() { docker exec "$N" sh -c 'grep VmRSS /proc/$(pidof java)/status' | awk '{printf "%.0f", $2/1024}'; }
cg() { docker stats --no-stream --format '{{.MemUsage}}' "$N" | cut -d/ -f1 | tr -d ' '; }
sleep 10
r_idle=$(rss) c_idle=$(cg)
session=$(python3 -I "$here/mcp-session.py" "$P" "$(cat "$SONARLESS_HOME/token")" "${PERF_PROJECT:-workit}")
sleep 2
r_load=$(rss) c_load=$(cg)
if [ -n "${HOOK:-}" ]; then bash -c "$HOOK" >"$L/hook.txt" 2>&1 || true; fi
docker stop -t 30 "$N" >/dev/null
docker logs "$N" >"$L/container.log" 2>&1
printf '%s ready_ms=%d rss_idle=%sMiB rss_after=%sMiB cgroup_idle=%s cgroup_after=%s %s\n' \
  "$label" $(((t1 - t0) / 1000000)) "$r_idle" "$r_load" "$c_idle" "$c_load" "$session" | tee -a "$PERF_DIR/logs/mcp-results.txt"
