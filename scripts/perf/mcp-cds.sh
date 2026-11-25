#!/usr/bin/env bash
# mcp-cds.sh "<JAVA_TOOL_OPTIONS>": build a static AppCDS archive for the MCP
# image in $PERF_DIR/cds/app.jsa. A training run (mcp-run.sh with
# -XX:DumpLoadedClassList) records the classes an agent session loads, then
# -Xshare:dump writes the archive with the same JVM options. The image's
# jlinked JDK has no base CDS archive, so -XX:+AutoCreateSharedArchive does
# nothing there. Use the archive with
#   mcp-run.sh cds "<opts> -XX:SharedArchiveFile=/cds/app.jsa" -v "$PERF_DIR/cds:/cds:ro"
# Source env.sh first.
set -euo pipefail
: "${PERF_DIR:?source scripts/perf/env.sh first}"
opts=$1
read -ra jvm <<<"$opts"
here=$(cd "$(dirname "$0")" && pwd)
image="${SONARLESS_MCP_IMAGE:-sonarsource/sonarqube-mcp:latest}"
mkdir -p "$PERF_DIR/cds"
chmod 777 "$PERF_DIR/cds" # the image runs as appuser (uid 100)
rm -f "$PERF_DIR/cds/classes.lst" "$PERF_DIR/cds/app.jsa"
"$here/mcp-run.sh" cds-train "$opts -XX:DumpLoadedClassList=/cds/classes.lst" -v "$PERF_DIR/cds:/cds"
docker run --rm --name "$SONARLESS_INSTANCE-cdsdump" -v "$PERF_DIR/cds:/cds" --entrypoint java "$image" \
  "${jvm[@]}" -Xshare:dump -XX:SharedClassListFile=/cds/classes.lst -XX:SharedArchiveFile=/cds/app.jsa \
  -cp /app/sonarqube-mcp-server.jar >"$PERF_DIR/logs/cds-dump.log" 2>&1
ls -l "$PERF_DIR/cds/app.jsa"
