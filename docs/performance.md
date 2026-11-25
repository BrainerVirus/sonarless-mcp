# Performance

Measurements behind the memory defaults, with the method so they can be
repeated. All runs used a throwaway instance (`SONARLESS_INSTANCE=perftest`,
its own `SONARLESS_HOME` and ports), never a real install. The scripts are in
[`scripts/perf/`](../scripts/perf).

Measured on 2026-11-25: Linux 6.8, 14 cores, 31 GiB RAM, Docker 29.10, SonarQube
`26.6.0.123539-community` (Java 25), `sonarsource/sonarqube-mcp:latest` built
2026-11-20 (a jlinked Java 21.0.13), sonar-scanner-cli 12.4.

## Summary

| Question | Result | Default |
|---|---|---|
| Is a 256m Elasticsearch heap enough for a large repo? | Yes. No OutOfMemoryError, no full GC, live old generation at most 72 MiB; same compute-engine time as 384m | `SONARLESS_SEARCH_HEAP=256m` kept |
| Where does the MCP JVM's memory outside the heap go? | Class metadata (71 MiB) and symbols (34 MiB) for 14.5k loaded classes; code cache 13 MiB; 82 threads 8 MiB | `SONARLESS_MCP_JAVA_OPTS` kept |
| Does AppCDS help the MCP JVM? | Yes: ready in 2.1 s instead of 3.9 s, java RSS 25 to 30 MiB lower, with the archive sonarless-mcp builds itself | `SONARLESS_MCP_APPCDS=true`: built in the background, used from the next start; see [AppCDS](#appcds-for-the-mcp-jvm) |

## Elasticsearch heap: 256m against 384m

**Method.** Scan a copy of the largest local repo the scanner actually
analyses (Workit at `bc1d174b`: 1508 files indexed, 972 analysed, 251,450 lines
of code, 3363 issues), once on a fresh server and again as a re-analysis, at
each heap. During each scan `jstat -gc` samples the Elasticsearch JVM every
second and `docker stats` samples the server container. After the scans, a
burst of 60 issue searches with facets (the queries the MCP issue tools send)
runs against the same index. The OutOfMemoryError, GC-overhead and
circuit-breaker counts come from `es.log`, `ce.log` and `web.log`.

| Run | Scan wall | Scanner total | Compute engine | Young GCs | Full GCs | Heap used, peak | Old gen, peak | Server container, peak |
|---|---|---|---|---|---|---|---|---|
| 256m, fresh | 190.8 s | 2:46 | 21.8 s | +1 (5 ms) | 0 | 164 MiB | 72 MiB | 2446 MiB |
| 256m, re-analysis | 174.9 s | 2:42 | 10.6 s | +1 (4 ms) | 0 | 196 MiB | 72 MiB | 2623 MiB |
| 256m, re-analysis after a recreate | 163.8 s | 2:26 | 15.4 s | +1 (6 ms) | 0 | 185 MiB | 42 MiB | 1989 MiB |
| 384m, fresh | 172.7 s | 2:30 | 20.9 s | +1 (4 ms) | 0 | 293 MiB | 120 MiB | 2494 MiB |
| 384m, re-analysis | 156.3 s | 2:23 | 11.1 s | 0 | 0 | 230 MiB | 120 MiB | 2659 MiB |
| 256m, 60 issue searches | 16.4 s | | | +2 (7 ms) | 0 | 211 MiB | 72 MiB | |
| 384m, 60 issue searches | 15.7 s | | | +2 (6 ms) | 0 | 322 MiB | 120 MiB | |

No run logged an OutOfMemoryError, a GC-overhead error or a tripped circuit
breaker. Elasticsearch spent under 40 ms in GC across a whole scan at either
size. The difference in wall time sits in the scanner's JavaScript/TypeScript
sensor (149, 147 and 129 s at 256m; 135 and 130 s at 384m), which runs in the
scanner container and never calls Elasticsearch: the third 256m run was as
fast as the 384m runs. The work Elasticsearch takes part in, the compute engine,
took the same time at both sizes. A larger heap only lets G1 leave more
garbage before it collects, which is why the peaks rise with the heap.

Elasticsearch's memory outside the heap (class metadata, code, threads) was
142 to 158 MiB at both sizes, and it maps index files outside the heap through
the page cache, so the heap is not its whole footprint.

## MCP JVM native memory

**Method.** `scripts/perf/mcp-nmt.sh` runs the MCP container with the default
`SONARLESS_MCP_JAVA_OPTS` plus `-XX:NativeMemoryTracking=summary`, runs an
agent-like session (initialize, `tools/list`, 12 tool calls: projects, quality
gate, measures, six pages of issues, three rules), then reads
`VM.native_memory summary`. The image ships only `java`, so `jcmd` runs from a
JDK 21 container that shares the MCP container's PID namespace and user
(`--pid container:<mcp> --user 100:101`).

| Area (committed) | Default | With AppCDS |
|---|---|---|
| Java heap | 64.0 MiB | 64.0 MiB |
| Metaspace (non-class metadata) | 71.0 MiB | 4.1 MiB |
| Symbol | 34.3 MiB | 2.3 MiB |
| Code (C1 only) | 13.2 MiB | 11.8 MiB |
| Class space and class malloc | 11.2 MiB | 2.7 MiB |
| Thread (82 threads, 512k stacks) | 8.2 MiB | 8.0 MiB |
| Shared class space (the mapped archive) | | 99.0 MiB |
| Internal, GC, compiler, other | 2.4 MiB | 2.4 MiB |
| NMT's own bookkeeping | 4.5 MiB | 1.4 MiB |
| Total committed | 209.2 MiB | 196.3 MiB |

At the moment of the read the JVM had 14,507 classes loaded (13,474 instance
classes and 1,033 array classes, NMT's count). They cost more than the heap:
metadata and symbols together are 105 MiB.

`MaxMetaspaceSize=128m` caps class metadata and class space together. The
table shows committed memory; NMT also reports what is used inside it:
metadata used 70.0 MiB (70.6 MiB committed; the table's 71.0 MiB adds the
category's own malloc) plus class space used 9.2 MiB (9.8 MiB committed) make
the 79 MiB of the cap in use. The cap only limits growth, so lowering it frees
nothing and only brings an OutOfMemoryError in Metaspace closer. The
thread count comes from the server's own pools, not from a JVM option.

Apart from AppCDS, no option to shrink metadata or symbols was measured. The
size follows the number of classes the server loads, and AppCDS is the one
change measured that moves both out of committed native memory: into a
read-only mapped file, of which only the pages touched are resident.

## AppCDS for the MCP JVM

**Method.** The image's jlinked JDK has no base CDS archive, so
`-XX:+AutoCreateSharedArchive` only warns
(`ArchiveClassesAtExit is unsupported when base CDS archive is not loaded`).
A static archive works (`scripts/perf/mcp-cds.sh`): a training run with
`-XX:DumpLoadedClassList` and the agent session writes a class list of 12,965
lines (the classes the JVM may archive, lambda entries included, recorded up
to the training run's exit), then `java -Xshare:dump` with the same options
writes a 101 MiB archive in about 5 s. In a run with the archive,
`-Xlog:class+load` logged 13,299 instance classes loaded up to the container's
stop, 12,443 of them from the archive. The three counts differ because each
is taken at a different point and counts different things: the list leaves
out classes the JVM cannot archive, the log counts instance classes only, and
NMT's 14,507 includes array classes. Each run starts a fresh container and measures
`docker run` to `/health` answering 200, then the java RSS (`VmRSS`) and the
container's memory (`docker stats`) 10 s after ready and after the agent
session.

| | Ready (median) | Java RSS idle | Java RSS after session | Container after session |
|---|---|---|---|---|
| Default options, 7 runs | 4.1 s (3.8 to 7.5) | 219 MiB | 231 MiB | 207 MiB |
| Default options + AppCDS, 6 runs | 2.0 s (1.4 to 2.5) | 191 MiB | 201 MiB | 171 MiB |

Tool latency did not change (every session's 12 calls finished in under 2 s).

With a stale archive (`scripts/perf/mcp-stale.sh`: the same jar with another
timestamp mounted over it) the JVM logs `[cds] ... timestamp has changed`, runs
without the archive and lands on the default numbers (ready 3.3 s, RSS
218 MiB), so a stale archive costs startup time, not correctness.

### As shipped

sonarless-mcp builds and uses the archive on its own
(`SONARLESS_MCP_APPCDS`, [Configuration](configuration.md#mcp-class-data-archive-appcds)).
The design follows from the measurements above and the ones below:

- **Never block a start.** The archive needs a training start and a dump
  (7 to 12 s on this machine), so the MCP container starts without one and a
  one-off container, `<instance>-cds`, builds it once the server answers. Its
  fixed name is the lock: a second process that tries to start a build gets
  Docker's name conflict and leaves it to the first. The next start of the MCP
  container uses the archive; a running container is never recreated for it.
- **What the training run loads.** The build starts the MCP server with
  `-XX:DumpLoadedClassList`, waits for `/health`, sends one `initialize`
  without a token (the request path up to the 401), and stops it. It never
  holds a SonarQube token, so it runs no agent session: that list has about
  12,000 classes against 12,965 for the session-trained one above, and costs
  about 5 MiB of java RSS after a session (205 against 201 MiB), nothing in
  ready time.
- **No SonarQube, no archive.** The MCP server does not answer `/health`
  without a reachable SonarQube (measured: no answer after 30 s, with a
  connection error to the configured URL), so a training start without one
  lists only the classes of a failed start. There is no fallback class list:
  the build only starts right after the MCP server got ready, and a training
  start that never answers fails the build, which is recorded and tried again
  after a day.
- **Keyed by image ID and JVM options.** The archive is named
  `<image ID, 12 hex>-<hash of SONARLESS_MCP_JAVA_OPTS>.jsa`, and the dump runs
  with exactly the options of the runs that map it. A new image or new options
  get a new archive; a successful build deletes every other one.
- **Atomic publish.** The dump writes a temporary file, a test run maps it
  with `-Xshare:on`, and only then is it renamed into place. A failed dump
  (a full disk, say) removes the temporary file and keeps the previous
  archive.
- **Fail open.** Runs use `-Xshare:auto`, never `on`: whatever is wrong with
  the archive, the JVM starts without it. sonarless-mcp reads the container's
  log from that start on, and a `[warning][cds]` line (the JVM's reason for
  not mapping the archive) records the archive as failed: the next start runs
  plain, and `status` shows the reason. A failed build or a rejected archive
  is tried again after a day, or at once for a new image or options.
- **A named volume**, `<instance>-cds`, mounted read-only into the MCP
  containers: not a bind mount, so it behaves the same on Docker Desktop and
  with Windows paths. The build container mounts it at the image's own
  `/app/storage`, so Docker gives the fresh volume that directory's owner and
  the image's user (appuser, 100:101) writes the archive without a root step.

**What the JVM does with a bad archive** (`-Xshare:auto`, the
`sonarsource/sonarqube-mcp` JDK 21.0.13, `java -version` with the archive):

| Archive | Without `-XX:+VerifySharedSpaces` | With it |
|---|---|---|
| Missing file | runs without it, logs nothing | the same |
| Truncated | `[warning][cds] The shared archive file has been truncated.`, runs without it | the same |
| First 4 bytes overwritten | `[warning][cds] The shared archive file has a bad magic number`, runs without it | the same |
| 4 bytes overwritten in the middle | **maps it** (says `sharing`) | `[warning][cds] Checksum verification failed.`, runs without it |
| Jar changed (another timestamp) | `[warning][cds] ... timestamp has changed.`, runs without it | the same |

Without the checksum a damaged archive is mapped as if it were whole.
`-XX:+VerifySharedSpaces` costs about 60 ms per JVM start (`java -version`
with the archive, 10 rounds of 5 runs: 76 to 86 ms without, 129 to 152 ms
with), against the 1.5 to 2 s the archive saves, so sonarless-mcp turns it on.
A missing file is the one case the JVM does not report. So after every start
with the archive sonarless-mcp also checks that the JVM mapped it: one
`docker exec <instance>-mcp sh -c 'grep -l -F /cds/<key>.jsa /proc/[0-9]*/maps'`
(busybox sh and grep are in the image; without them it reads the init's
`/proc/1/task/1/children` and the JVM's `/proc/<pid>/maps` with `cat`). Only a
start seen mapping the archive is reported as `used`; one that did not map it
records the archive as failed. A removed volume is caught earlier: the start
checks that the volume exists before it mounts it, and builds again when not.

**Shipped numbers.** The archive the `<instance>-cds` container built
(11,698 classes, 95 MiB), run the way the MCP container runs it
(`-XX:SharedArchiveFile=... -Xshare:auto -XX:+VerifySharedSpaces`, the volume
mounted read-only), against the default options, alternating, 6 runs each with
`scripts/perf/mcp-run.sh`:

| | Ready (median) | Java RSS idle | Java RSS after session | Container after session |
|---|---|---|---|---|
| Default options, 6 runs | 3.9 s (3.4 to 5.7) | 219 MiB | 230 MiB | 206 MiB |
| Default options + the built archive, 6 runs | 2.1 s (1.7 to 2.4) | 189 MiB | 205 MiB | 171 MiB |

One archive run reported 247 MiB for the container after the session (the
other five 171 to 172 MiB) with the same java RSS; the page cache Docker
charges to a container's memory includes the mapped archive's pages when the
container is the first to read them.

The live checks on the perftest instance, with the built binary: no archive
gave a plain start and a build in the background (7 s); after a stop the
container was recreated with the archive (`/proc/<java>/maps` showed it
mapped; `status`: `used`); 4 bytes overwritten in the archive gave
`Checksum verification failed` in the log, a plain start, `status`:
`failed`, and no rebuild on the following start; deleting the record file
rebuilt it; `SONARLESS_MCP_APPCDS=false` recreated the container without the
archive and started no build; another MCP image got its own key, a plain
start and its own build, which replaced the other image's archive. With the
mapping probe: a start with the archive recorded that start as mapped
(`appcds`: `used`); the archive file deleted from the volume (the volume
kept) gave `failed: the JVM did not map /cds/<key>.jsa`; `appcds rebuild`
built it again in about 10 s, and the next start was `used` again.

## Repeat the measurements

Sourcing `scripts/perf/env.sh` points `HOME`, the `XDG_*` dirs and every
`SONARLESS_*` setting of the current shell at `PERF_DIR`, so run everything in
a subshell. Each script also checks on its own that `SONARLESS_INSTANCE`
contains `perftest` and that `SONARLESS_HOME` is under `PERF_DIR`, and stops
before any docker call if not.

```sh
export PERF_DIR=$(mktemp -d)
go build -buildvcs=false -o "$PERF_DIR/sonarless-mcp" ./cmd/sonarless-mcp
mkdir "$PERF_DIR/repo"           # scans write .scannerwork: scan a copy
git -C <repo> archive HEAD | tar -x -C "$PERF_DIR/repo"
(
  . scripts/perf/env.sh          # perftest instance, ports 19233 to 19236
  sl start
  scripts/perf/es-scan.sh es256 "$PERF_DIR/repo"
  scripts/perf/es-burst.sh es256-burst <project key>
  SONARLESS_SEARCH_HEAP=384m sl start            # recreates the server, data kept
  SONARLESS_SEARCH_HEAP=384m scripts/perf/es-scan.sh es384 "$PERF_DIR/repo"
  opts='-Xmx64m -XX:+UseSerialGC -XX:TieredStopAtLevel=1 -XX:ReservedCodeCacheSize=32m -XX:MaxMetaspaceSize=128m -Xss512k'
  scripts/perf/mcp-run.sh base "$opts"
  scripts/perf/mcp-nmt.sh nmt "$opts"
  scripts/perf/mcp-cds.sh "$opts"
  scripts/perf/mcp-run.sh cds "$opts -XX:SharedArchiveFile=/cds/app.jsa" -v "$PERF_DIR/cds:/cds:ro"
  scripts/perf/mcp-nmt.sh nmt-cds "$opts -XX:SharedArchiveFile=/cds/app.jsa" -v "$PERF_DIR/cds:/cds:ro"
  scripts/perf/mcp-stale.sh "$opts"
  # As shipped: let sonarless-mcp build the archive (start the MCP once, wait
  # for perftest-cds to exit), then compare it with the default options.
  key=<the key in $SONARLESS_HOME/cache/perftest-cds.json>
  scripts/perf/mcp-run.sh plain "$opts"
  scripts/perf/mcp-run.sh appcds "$opts -XX:SharedArchiveFile=/cds/$key.jsa -Xshare:auto -XX:+VerifySharedSpaces" \
    --mount type=volume,src=perftest-cds,dst=/cds,readonly
)
```

Afterwards remove only the perftest resources: the containers `perftest-server`,
`perftest-mcp`, `perftest-mcpx` and `perftest-cds` (with `docker rm -fv`, which
also drops the server's anonymous `/opt/sonarqube/temp` volume), the network
`perftest` and the volumes `perftest-server-<version>-{data,extensions,logs}`
and `perftest-cds`.
