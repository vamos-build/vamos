# Manual smoke checks

Run from the repository root in the [development environment](development.md).
Required change scopes and other validation are in [testing.md](testing.md).
Create a temporary directory for the tarballs and traces:

```sh
TMP=$(mktemp -d)
```

The MCP checks send `examples/typescript-mcp/echo-request.jsonl` over stdin and should end with its reply:
`{"result":{"content":[{"type":"text","text":"hola"}]},"jsonrpc":"2.0","id":2}`.

```
# 1. Command line: help works; mistakes print `vamos: ...` and exit 1
go build -o vamos . && ./vamos --help && ./vamos nope; ./vamos load

# 2. load into Docker, then run
./vamos load server vamos-example -C examples/typescript-mcp \
  && docker run -i --rm vamos-example < examples/typescript-mcp/echo-request.jsonl

# 3. save a tarball, then load and run it (use a temp dir, not the repo)
./vamos save server "$TMP/example.tar" -t vamos-example:tar -C examples/typescript-mcp \
  && docker load -i "$TMP/example.tar" && docker run -i --rm vamos-example:tar \
  < examples/typescript-mcp/echo-request.jsonl

# 4. The release image.
# Paths in the devcontainer aren't host paths, so pipe the example in instead of bind-mounting it.
docker build -q . -t vamos \
  && tar -C examples/typescript-mcp --exclude=node_modules -c . | docker run -i --rm --privileged \
  -v /var/run/docker.sock:/var/run/docker.sock --entrypoint sh vamos \
  -c 'mkdir /example && tar -x -C /example && vamos load server vamos-example:image -C /example' \
  && docker run -i --rm vamos-example:image < examples/typescript-mcp/echo-request.jsonl

# 5. The fastmcp example.
# Its SDK cancels pending requests when stdin closes, so hold stdin open long enough for it to start.
./vamos load server vamos-fastmcp -C examples/fastmcp \
  && (cat examples/typescript-mcp/echo-request.jsonl; sleep 6) | docker run -i --rm vamos-fastmcp

# 6. The strawberry-react workspace, behind its compose file's ingress.
# The devcontainer can't reach the host's port 8080, so curl from the compose network.
# Expect the page's <title>, then the reply below.
S=examples/strawberry-react
./vamos load //backend:layer strawberry-backend -C $S && ./vamos load //ui:layer strawberry-ui -C $S \
  && docker compose -f $S/docker-compose.yaml up -d --wait \
  && curl="docker run --rm --network strawberry-graphql_default curlimages/curl -s" \
  && $curl http://ingress/ | grep '<title>' \
  && $curl http://ingress/graphql -H 'content-type: application/json' \
  -d '{"query":"mutation { increment { value } }"}'; \
  docker compose -f $S/docker-compose.yaml down
```

Check 6's reply is `{"data":{"increment":{"value":1}}}`.

```
# 7. The golang-cli example: an index, cross-compiled; load sends the daemon's platform's image.
# Its pull is pinned in vamos.lock, which the builds require, so that they write no lockfile.
# Expect "hola", from each platform's image.
VAMOS_PINS=require ./vamos load echo vamos-echo -C examples/golang-cli && docker run --rm vamos-echo \
  && VAMOS_PINS=require ./vamos save //:layer "$TMP/echo.tar" -t vamos-echo:other --platform linux/amd64 \
  -C examples/golang-cli && docker load -i "$TMP/echo.tar" && docker run --rm --platform linux/amd64 vamos-echo:other
# Optionally, whether the pin is still what the tag names at Docker Hub: a newer 1.27.x-alpine there fails it.
./vamos update --dry-run -C examples/golang-cli
```

```
# 8. export: the strawberry-react UI's generated GraphQL types, written to a temp dir, then again over them.
# Expect src/gql's files, owned by you, and "it isn't empty" from the second, until --replace.
S=examples/strawberry-react
./vamos export //ui:graphql-codegen "$TMP/gql" -C $S && ls -ln "$TMP/gql" \
  && ./vamos export //ui:graphql-codegen "$TMP/gql" -C $S; ./vamos export //ui:graphql-codegen "$TMP/gql" \
  --replace -C $S
```

```
# 9. A trace, queried with Perfetto's trace processor, which the devcontainer has.
# Expect the slowest spans, then arrows from what each glob, pull or target made to the steps that use it:
# none if every target was reused.
./vamos load server vamos-example -C examples/typescript-mcp --trace "$TMP/trace.json" \
  && echo "select name, dur/1000000 as ms from slice order by dur desc limit 10;
           select s1.name, s2.name from flow join slice s1 on slice_out = s1.id join slice s2 on slice_in = s2.id;" \
  | trace_processor_shell -q /dev/stdin "$TMP/trace.json"
```

```sh
# 10. The introductory tutorial's TypeScript UI: build, load, serve, and export.
# Expect the heading and bundled message over HTTP, matching the exported website.
# The script checks inside the container, so it also works from the devcontainer.
./examples/typescript-ui/check.sh
```

```sh
# 11. install.sh, as the release's smoke test runs it: the tutorial's devcontainer Dockerfile on Ubuntu 24.04,
# installing this platform's release binary from a local directory rather than a release, then the MCP example.
# Expect "vamos version v0.0.0-test", then the MCP reply.
shellcheck -s sh install.sh && mkdir -p "$TMP/devc" \
  && docker build -q --target binary --build-arg VERSION=v0.0.0-test --output "$TMP/devc/dist" . \
  && mv "$TMP/devc/dist/vamos" "$TMP/devc/dist/vamos-linux-$(go env GOARCH)" && cp install.sh "$TMP/devc/dist/" \
  && (cd "$TMP/devc/dist" && sha256sum vamos-linux-* > SHA256SUMS) \
  && awk '/^```dockerfile/ { f = 1; next } /^```/ { f = 0 } f' docs/user/tutorial.md \
  | sed 's#^RUN curl -fsSL -o /tmp/vamos-install.sh https://github.com/vamos-build/vamos/releases/download/[^ ]*/install.sh \\$#COPY dist /tmp/dist\nENV VAMOS_RELEASE_URL=file:///tmp/dist\nRUN curl -fsSL -o /tmp/vamos-install.sh file:///tmp/dist/install.sh \\#' \
  > "$TMP/devc/Dockerfile" && grep -qF 'file:///tmp/dist/install.sh' "$TMP/devc/Dockerfile" \
  && docker build -q -t vamos-devcontainer "$TMP/devc" \
  && docker run --rm vamos-devcontainer vamos --version \
  && tar -C examples/typescript-mcp --exclude=node_modules -c . | docker run -i --rm --privileged \
  -v /var/run/docker.sock:/var/run/docker.sock vamos-devcontainer \
  sh -c 'mkdir /example && tar -x -C /example && vamos load server vamos-example:install -C /example' \
  && docker run -i --rm vamos-example:install < examples/typescript-mcp/echo-request.jsonl
```

`push` has no manual check here:
the tests already push to a real (in-process) registry, from the executor and from the client.
`publish` has one, since its scenarios publish small images alone:

```sh
# 12. publish: the strawberry-react release to a registry of its own, from the executor, then from the client.
# Expect an "Index built!" line for each tag of strawberry-ui and of strawberry-backend,
# the client's naming what the executor's did, and then the tags 1.0, 1.1 and latest in each repository.
# The backend's other platform builds under emulation, through the host's binfmt_misc handlers,
# so the host needs an emulator for it, as development.md says.
C=$(docker run -d --rm registry:3) \
  && R=$(docker inspect "$C" --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}'):5000 \
  && printf '[[registry]]\nlocation = "%s"\ninsecure = true\n' "$R" > "$TMP/registries.conf" \
  && S=examples/strawberry-react \
  && CONTAINERS_REGISTRIES_CONF="$TMP/registries.conf" ./vamos publish //:release --tag 1.0 --tag latest \
  --set registry=$R/strawberry -C $S \
  && ./vamos publish //:release --tag 1.1 --set registry=$R/strawberry --push-from=client --tls-verify=false -C $S \
  && curl -fs http://$R/v2/strawberry/strawberry-ui/tags/list | grep '"1.0".*"1.1".*"latest"' \
  && curl -fs http://$R/v2/strawberry/strawberry-backend/tags/list | grep '"1.0".*"1.1".*"latest"'; \
  status=$?; [ -n "$C" ] && docker stop "$C"; (exit $status)
```

## Remote execution

The [remote environment](remote-environment.md) runs the coordinator, two workers and the registries that remote
builds share.

```sh
# R1. The registries come up, answer by name from the devcontainer, and the pull-through cache serves Docker Hub.
# Expect two 200s, then 405: the proxy refuses pushes.
docker compose -f remote/compose.yaml up -d --wait \
  && docker network connect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")" \
  && curl -s -o /dev/null -w 'registry %{http_code}\n' http://registry:5000/v2/ \
  && curl -s -o /dev/null -w 'hub-proxy %{http_code}\n' \
    -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json' \
    http://hub-proxy:5000/v2/library/busybox/manifests/1.36 \
  && curl -s -X POST -o /dev/null -w 'hub-proxy push %{http_code}\n' http://hub-proxy:5000/v2/x/blobs/uploads/; \
  docker network disconnect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")"; \
  docker compose -f remote/compose.yaml down

# R2. A cold build on the coordinator, loaded into Docker from the cache registry, then a warm one: expect the MCP
# reply, then every target cached. The client reads images from the registry over plain HTTP, which its registry
# settings must allow. The images are built from the Dockerfile, so this takes a few minutes the first time.
export CONTAINERS_REGISTRIES_CONF=$PWD/remote/registries.conf
docker compose -f remote/compose.yaml up -d --build --wait \
  && docker network connect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")" \
  && ./vamos load server vamos-example:remote -C examples/typescript-mcp --coordinator http://coordinator:8080 \
  && docker run -i --rm vamos-example:remote < examples/typescript-mcp/echo-request.jsonl \
  && ./vamos build server -C examples/typescript-mcp --coordinator http://coordinator:8080; \
  docker network disconnect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")"; \
  docker compose -f remote/compose.yaml down

# A coordinator refuses a prune for a lease (30s by default) after it starts, and while any build is held by a
# client that hasn't released it; a prune in these checks waits that long after `up`.
# The fault checks need a target that runs long enough to interrupt: with the proxy warm, npm install takes a
# second, so they use a project of their own whose layer sleeps, and leaves a daemon behind with setsid.
mkdir -p "$TMP/slow" && printf '%s\n' \
  'pull(name="busybox", image="docker.io/library/busybox", tag="1.36")' \
  'layer(name="slow", base="busybox", steps=[run(["sh", "-c", "setsid sleep 300 & sleep 45"])])' \
  > "$TMP/slow/BUILD"

# R3. A dropped connection: take this container off the coordinator's network while a build runs, for longer than
# the client's minute-long wait for a line of the event stream, and expect warnings that the connection dropped and
# came back, then the build's success: the worker finished the target meanwhile, and the resumed watch finds it.
# A blip shorter than that wait proves nothing, since the stream's TCP connection survives it.
# Setup runs first, and only the network's timer in the background, as in R6.
docker compose -f remote/compose.yaml up -d --wait \
  && docker network connect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")" \
  && { (sleep 10; docker network disconnect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")"; sleep 75; \
        docker network connect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")") & \
       ./vamos build slow --rebuild -C "$TMP/slow" --coordinator http://coordinator:8080; wait; }; \
  docker network disconnect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")"; \
  docker compose -f remote/compose.yaml down

# R4. A worker killed mid-build: the build's running target is tried again on the other worker and the build
# succeeds, its trace holding the try that was lost, before the one that ran; the worker comes back, logs that it
# registered again, and cleans up after
# the attempt it was running. `docker restart -t 0` kills the process at once and starts the container again;
# `docker kill` would leave it stopped, since Docker's restart policy ignores a manual kill. Workers take work as
# they poll, so the attempt may be on worker-2: if the trace holds no lost try, run the check again restarting that one.
# Setup runs first, and only the restart's timer in the background, as in R6.
docker compose -f remote/compose.yaml up -d --wait \
  && docker network connect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")" \
  && { (sleep 15; docker restart -t 0 remote-worker-1) & \
       ./vamos build slow --rebuild -C "$TMP/slow" --coordinator http://coordinator:8080 --trace "$TMP/r4.json"; \
       wait; }; \
  grep -oE '"outcome": *"lost"' "$TMP/r4.json"; \
  docker compose -f remote/compose.yaml logs worker | grep -i "registered\|cleaning up"; \
  docker network disconnect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")"; \
  docker compose -f remote/compose.yaml down

# R5. Ctrl-C kills the running attempt on the worker, and every process it started: expect no attempt cgroup
# left on either worker a few seconds after interrupting the build (the release image has no ps; the cgroup
# directory under /sys/fs/cgroup/vamos is the attempt, and it is removed once its processes are gone).
docker compose -f remote/compose.yaml up -d --wait \
  && docker network connect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")" \
  && (./vamos build slow --rebuild -C "$TMP/slow" --coordinator http://coordinator:8080 & \
      sleep 15; kill -INT $!; wait; sleep 5; \
      for i in 1 2; do docker compose -f remote/compose.yaml exec --index $i worker ls /sys/fs/cgroup/vamos; done); \
  docker network disconnect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")"; \
  docker compose -f remote/compose.yaml down

# R6. The coordinator restarted mid-build resumes it: expect warnings that the connection dropped and came back and
# that the coordinator restarted, then the build's success. The slow layer starts again, so the build
# takes its 45 seconds again after the restart; the workers log that they registered again.
# Setup runs first, and only the restart's timer in the background, so the build starts once the network is joined.
docker compose -f remote/compose.yaml up -d --wait \
  && docker network connect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")" \
  && { (sleep 15; docker restart remote-coordinator-1) & \
       ./vamos build slow --rebuild -C "$TMP/slow" --coordinator http://coordinator:8080; wait; }; \
  docker compose -f remote/compose.yaml logs worker | grep -i "registering again"; \
  docker network disconnect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")"; \
  docker compose -f remote/compose.yaml down

# R7. Telemetry through an OpenTelemetry Collector: with one added for the check by an override file, and the
# coordinator and workers exporting to it, a build whose client sets TRACEPARENT puts the coordinator's `build` span
# under the given parent, in its trace, and the collector logs the coordinator's and workers' metrics, with exemplars of
# that trace, naming one of its tries, on the attempts' histogram. The IDs are new for each run, and the project is
# taken down first with its volumes, so that nothing an earlier run left, a log, a counter or a job its coordinator
# would resume, can pass; the build is given ten minutes. The collector's debug exporter logs everything it receives, a
# span or a metric as a block of `-> Field: value` lines; the metrics are exported every 5 seconds, so the log is read
# again, each read given 20 seconds, until all is there or a minute has passed. A run interrupted before it left the
# project's network has left this container on it, which `down` can't remove, so R7 leaves it first. R7 runs as a
# Compose project of its own, vamos-r7, so that its volumes can go without the shared project's cache registry and
# Docker Hub mirror, though its own mirror then starts empty and pulls from Docker Hub, and its images are removed at
# the end. Its coordinator is reached as r7-coordinator, a name the shared project's network doesn't answer to if this
# container is still on it.
cat > "$TMP/otel.yaml" <<'YAML'
services:
  coordinator:
    environment: {OTEL_EXPORTER_OTLP_ENDPOINT: "http://collector:4318", OTEL_METRIC_EXPORT_INTERVAL: "5000"}
    depends_on: {collector: {condition: service_started}}
    networks: {default: {aliases: [r7-coordinator]}}
  worker:
    environment: {OTEL_EXPORTER_OTLP_ENDPOINT: "http://collector:4318", OTEL_METRIC_EXPORT_INTERVAL: "5000"}
  collector:
    image: otel/opentelemetry-collector:0.165.0
    command: ["--config=/etc/otel.yaml"]
    configs: [{source: otel, target: /etc/otel.yaml}]
configs:
  otel:
    content: |
      receivers: {otlp: {protocols: {http: {endpoint: "0.0.0.0:4318"}}}}
      exporters: {debug: {verbosity: detailed}}
      service:
        pipelines:
          traces: {receivers: [otlp], exporters: [debug]}
          metrics: {receivers: [otlp], exporters: [debug]}
YAML
# The images are built again, as in R2, since a check of a change needs its own code. The script fails if a check does.
R7=ok
T=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n'); S=$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')
docker network disconnect vamos-r7_default "vamos_$(basename "$WORKSPACE_FOLDER")" 2>/dev/null
docker compose -p vamos-r7 -f remote/compose.yaml -f "$TMP/otel.yaml" down -v \
  && docker compose -p vamos-r7 -f remote/compose.yaml -f "$TMP/otel.yaml" up -d --build --wait \
  && docker network connect vamos-r7_default "vamos_$(basename "$WORKSPACE_FOLDER")" \
  && TRACEPARENT="00-$T-$S-01" \
     timeout 600 ./vamos build slow --rebuild -C "$TMP/slow" --coordinator http://r7-coordinator:8080 \
  || R7=failed
end=$(($(date +%s) + 60)); missing=" run, as it failed"
while test "$R7" = ok; do
  missing=" log"
  if timeout 20 docker compose -p vamos-r7 -f remote/compose.yaml -f "$TMP/otel.yaml" logs collector \
      > "$TMP/r7.txt"; then
    missing=
    # A span whose trace, parent and name are the run's, read within each span's block.
    awk -v t="$T" -v s="$S" '/Span #|Metric #/ { tr = pa = na = "" }
      /Trace ID *:/ { tr = $NF } /Parent ID *:/ { pa = $NF }
      /Name *:/ { na = $0; sub(/^[^:]*Name *: /, "", na) }
      tr == t && pa == s && na == "build" { found = 1 } END { exit !found }' "$TMP/r7.txt" \
      || missing="$missing build span"
    for m in 'jobs\.duration' workers 'worker\.fenced'; do
      grep -qE "Name: vamos\.$m\$" "$TMP/r7.txt" || missing="$missing vamos.$m"
    done
    # An exemplar on the attempts' histogram naming the run's trace and one of its tries: the log is read twice, for
    # the spans and then for the exemplars, each within its block.
    awk -v t="$T" 'NR == FNR { if (/Span #|Metric #/) tr = sp = ""
      if (/Trace ID *:/) tr = $NF; if (/(^|[|]) +ID +:/) sp = $NF
      if (/-> vamos\.arg\.role: Str\(try\)/ && tr == t) id[sp]; next }
      /Metric #/ { me = "" } /-> Name: / { me = $NF } /Exemplar #/ { et = "" } /-> Trace ID: / { et = $NF }
      me == "vamos.attempts.duration" && /-> Span ID: / && et == t && ($NF in id) { found = 1 } END { exit !found }' \
      "$TMP/r7.txt" "$TMP/r7.txt" \
      || missing="$missing exemplar"
  fi
  test -z "$missing" || test "$(date +%s)" -ge "$end" && break
  sleep 5
done
test -z "$missing" || { echo "R7: no$missing"; R7=failed; }
docker network disconnect vamos-r7_default "vamos_$(basename "$WORKSPACE_FOLDER")"
docker compose -p vamos-r7 -f remote/compose.yaml -f "$TMP/otel.yaml" down -v --rmi local || R7=failed
test "$R7" = ok
```

## Kubernetes

The Helm chart's checks run on the kind cluster of the [remote environment](remote-environment.md#kubernetes),
installed as that section shows, with the slow project of the checks above and the coordinator forwarded to
`127.0.0.1:18080`. Run each check's lines as one script: a client started in the background of one shell and watched
from another is easily killed with it.

```sh
# K1. The chart renders, and what it renders is valid Kubernetes; expect no errors and every resource valid.
helm lint --strict remote/helm/vamos \
  && helm template vamos remote/helm/vamos | kubeconform -strict -summary

# K2. A worker killed mid-build, as R4: the build succeeds with a retry, and the worker comes back in the same pod,
# cleaning up after its attempt. Each worker has a cgroup namespace of its own, so the attempt's cgroup is in the
# pod that runs it alone, which picks the victim.
./vamos build slow --rebuild -C "$TMP/slow" --coordinator http://127.0.0.1:18080 --trace "$TMP/k2.json" & B=$!
sleep 15
for p in $(kubectl -n vamos get pods -o name | grep worker); do
  kubectl -n vamos exec $p -- nsenter -t 1 -C -m ls /sys/fs/cgroup/vamos | grep -q attempt && V=$p
done
docker exec vamos-control-plane crictl stop --timeout 0 \
  "$(kubectl -n vamos get $V -o jsonpath='{.status.containerStatuses[0].containerID}' | sed 's|containerd://||')"
wait $B; grep -oE '"outcome": *"lost"' "$TMP/k2.json"; sleep 5; kubectl -n vamos logs $V | grep -i "registered\|cleaning up"

# K3. The coordinator's pod deleted mid-build, as R6: the StatefulSet starts another on the same volume claim,
# which resumes the build, and the client, reconnected through a new port-forward, sees it succeed.
# A port-forward ends with the pod it forwards to, so the check stops the section's, runs its own, and starts it
# again once the new pod is ready; start the section's again after it.
pkill -f "^([^ ]*/)?kubectl .*port-forward svc/vamos-coordinator 18080:"
kubectl -n vamos port-forward svc/vamos-coordinator 18080:8080 >/dev/null 2>&1 & P=$!
./vamos build slow --rebuild -C "$TMP/slow" --coordinator http://127.0.0.1:18080 & B=$!
sleep 15; kubectl -n vamos delete pod vamos-coordinator-0
kubectl -n vamos wait --for=condition=Ready pod/vamos-coordinator-0 --timeout=120s
kill $P; kubectl -n vamos port-forward svc/vamos-coordinator 18080:8080 >/dev/null 2>&1 & P=$!
wait $B; kill $P

# K4. Ctrl-C kills the running attempt, as R5: expect no attempt cgroup left on any worker.
./vamos build slow --rebuild -C "$TMP/slow" --coordinator http://127.0.0.1:18080 & B=$!
sleep 15; kill -INT $B; wait $B; sleep 5
for p in $(kubectl -n vamos get pods -o name | grep worker); do
  kubectl -n vamos exec $p -- nsenter -t 1 -C -m ls /sys/fs/cgroup/vamos | grep attempt
done

# K5. Prometheus on: with the release upgraded, keeping its values and taking the chart's new defaults, its coordinator
# serves its metrics through the Service's `metrics` port, apart from the API's; after a build, expect each of a job
# counted, the pool's two workers of its platform and the API's requests by route, and a 404 for /metrics on the API's
# port; the script fails if any is missing, and after a failed upgrade, rollout or build skips the rest. It starts from
# a release with Prometheus off, as installed above, since only then does the upgrade restart the coordinator, which a
# rerun needs to check what the new pods serve: turn it off again first. The upgrade restarts the coordinator, and a
# worker notices only when its wait for an assignment fails, up to a lease later, so the build runs its layer again,
# which needs a worker, and the scrape is tried again every 5 seconds until one succeeds with both registered, starting
# none after 90 seconds and giving each 20. The scrapes run in one pod, started first, whose image pull has two minutes
# of its own. K5 stops the section's port-forward as K3 does, once its upgrade has succeeded; start it again after K5.
K5=ok; P=
helm upgrade vamos remote/helm/vamos -n vamos --wait --reset-then-reuse-values --set telemetry.prometheus.enabled=true \
  && kubectl -n vamos wait --for=condition=Ready pod/vamos-coordinator-0 --timeout=120s \
  || { echo "K5: the upgrade or the coordinator's rollout failed"; K5=failed; }
if test "$K5" = ok; then
  pkill -f "^([^ ]*/)?kubectl .*port-forward svc/vamos-coordinator 18080:"
  kubectl -n vamos port-forward svc/vamos-coordinator 18080:8080 >/dev/null 2>&1 & P=$!
  # The build waits for a worker as long as none is registered, so it is given ten minutes.
  sleep 2; timeout 600 ./vamos build slow --rebuild -C "$TMP/slow" --coordinator http://127.0.0.1:18080 \
    || { echo "K5: the build failed"; K5=failed; }
fi
if test "$K5" = ok; then
  # Scraped as Prometheus would, from a pod, through the Service's metrics port, rather than through a port-forward.
  M=$(kubectl -n vamos get svc vamos-coordinator --request-timeout=30s \
    -o jsonpath='{.spec.ports[?(@.name=="metrics")].port}')
  missing=" metrics port on the Service"; end=
  if test -n "$M"; then
    missing=" scraper pod"
    kubectl -n vamos delete pod k5-scrape --ignore-not-found --grace-period=0 --force --request-timeout=30s 2>/dev/null
    kubectl -n vamos run k5-scrape --request-timeout=30s --restart=Never --image=curlimages/curl:8.23.0 \
        --command -- sleep 600 >/dev/null \
      && kubectl -n vamos wait --for=condition=Ready pod/k5-scrape --timeout=120s >/dev/null \
      && end=$(($(date +%s) + 90))
  fi
  while test -n "$end" && test "$(date +%s)" -lt "$end"; do
    missing=" successful scrape"
    if timeout 20 kubectl -n vamos exec k5-scrape -- curl -fsS --max-time 10 "http://vamos-coordinator:$M/metrics" \
        > "$TMP/k5.txt"; then
      missing=
      grep -qE '^vamos_jobs_duration_seconds_count\{[^}]*outcome="succeeded"[^}]*\} [1-9]' "$TMP/k5.txt" \
        || missing="$missing succeeded job"
      grep -qE '^vamos_workers\{[^}]*platform="linux/[a-z0-9]+"[^}]*\} 2$' "$TMP/k5.txt" \
        || missing="$missing two workers"
      grep -qE 'http_route="/v1/jobs/\{id\}/events"' "$TMP/k5.txt" || missing="$missing route of the API"
    fi
    test -z "$missing" && break
    sleep 5
  done
  kubectl -n vamos delete pod k5-scrape --ignore-not-found --grace-period=0 --force --request-timeout=30s \
    2>/dev/null
  test -z "$missing" || { echo "K5: no$missing"; K5=failed; }
  test "$(curl -s --max-time 10 -o /dev/null -w '%{http_code}' http://127.0.0.1:18080/metrics)" = 404 \
    || { echo "K5: /metrics on the API"; K5=failed; }
fi
test -z "$P" || kill $P; test "$K5" = ok
```

Remove the temporary artifacts after the checks: `rm -rf -- "$TMP"`.
