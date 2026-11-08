# Remote execution environment

The design is explained, with diagrams, in [docs/design/remote-execution.md](../../docs/design/remote-execution.md):
the coordinator, workers and registries, the worker API, leases and fencing, restarts, and the Kubernetes chart.
This file says how to run the environment for development and testing.

[remote/compose.yaml](../../remote/compose.yaml) runs that environment with Docker Compose in the devcontainer,
so that each step of remote execution is tested against real registries.
The file runs the coordinator, two workers, both built from the release [Dockerfile](../../Dockerfile), and the
registries: `registry:5000`, whose repository `vamos-cache` holds built layers, and `hub-proxy:5000`,
the pull-through cache for Docker Hub.
A client in the devcontainer names the coordinator with `--coordinator http://coordinator:8080` once it has joined
the network; `docker compose up --scale worker=3` runs more workers.
Run the commands below from the repository root.

| Service | Compose specifics |
| --- | --- |
| `coordinator` | Not privileged; a volume for `/var/lib/vamos`, which keeps its jobs too, so that a restart resumes them; its registry settings are the compose file's inline copy of `remote/registries.conf`, since a devcontainer path can't be mounted |
| `worker` | Privileged, as buildah needs; an anonymous volume for its container store, a disposable cache; two replicas |
| `registry` | Deletion enabled, so a prune can remove tags; blob space returns only when the registry's garbage collection runs |
| `hub-proxy` | Proxy mode refuses pushes (405) |

Nothing publishes a port: the devcontainer can't reach ports published on the host,
so a client joins the project's network and reaches the services by name.
Both registries speak plain HTTP without authentication.
[remote/registries.conf](../../remote/registries.conf) marks them insecure for containers/image;
the services get the same settings as an inline compose config, and a client in the devcontainer points
`CONTAINERS_REGISTRIES_CONF` at the file, which governs the client's own registry access as well as the local
executor's, so `--tls-verify=false` isn't needed besides.
The client needs them to deliver: `load` and `save` read built images from `registry:5000` and pulled bases from
`hub-proxy:5000`, as the results locate them, through [finish](../../internal/finish/doc.go)'s `Image`.

```sh
docker compose -f remote/compose.yaml up -d --build --wait  # the project is named "remote"
docker network connect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")"
curl -s http://registry:5000/v2/                             # {} once the registry answers
./vamos build server -C examples/typescript-mcp --coordinator http://coordinator:8080
docker network disconnect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")"
docker compose -f remote/compose.yaml down -v                # -v drops the registries' contents too
```

The project name is the same from every worktree, so compose treats the environment of one worktree
and another as the same project: run it from one worktree at a time, or pass `-p` to name a second.
Each `up` after a `down -v` pulls base images through the proxy anew, which Docker Hub counts
against its anonymous pull allowance; a `down` without `-v` keeps the registries' contents but also leaves the
workers' anonymous container-store volumes behind, one per worker per `up`, so prefer `down -v` unless the next
run should find the registries warm, and `docker volume prune` now and then.
The coordinator offers `${VAMOS_PLATFORM:-linux/amd64}`: on an arm64 host, export `VAMOS_PLATFORM=linux/arm64`
before `up`, or a client on that host builds for a platform the workers never serve and waits.

Fault injection uses Docker alone:
a worker is lost with `docker restart -t 0`, which kills its process and brings it back, since Docker's restart
policy ignores a `docker kill`; a client is disconnected with `docker network disconnect`; and the coordinator
restarts with `docker restart`.
The checks themselves are in [smoke-checks.md](smoke-checks.md).

## Kubernetes

[remote/helm/vamos](../../remote/helm/vamos/values.yaml) is a Helm chart for the same services on a cluster,
with each object named after the release, as `vamos-coordinator:8080`, `vamos-registry:5000` and
`vamos-hub-proxy:5000` for a release named `vamos`.
Its values file documents every setting.
Clients outside the cluster reach those names only if the values give `cacheRegistry.address` and each cache's
`address` as names that clients resolve as well;
`kubectl port-forward` gives a client outside the cluster the coordinator, which is all `vamos build` needs.

The devcontainer has `kind`, `kubectl`, `helm` and `kubeconform`.
kind runs the cluster's node as a container on the host's Docker, on a network named `kind`,
which the devcontainer joins to reach the API server and the node, as it joins the compose network.
A chart under development runs the working tree's release image rather than the published one,
so it is built, loaded into the node as `vamos:latest` and named with `image.repository=vamos`;
on an arm64 host the pool is `linux/arm64`.

```sh
kind create cluster --name vamos                             # once; skip if `kind get clusters` lists it
docker network connect kind "vamos_$(basename "$WORKSPACE_FOLDER")"
kind export kubeconfig --name vamos --internal               # the API server by its name on the kind network
docker build -q . -t vamos:latest && kind load docker-image vamos:latest --name vamos
helm install vamos remote/helm/vamos -n vamos --create-namespace --wait --set image.repository=vamos \
  --set workers.pools.amd64=null --set workers.pools.arm64.platform=linux/arm64   # on an arm64 host
helm test vamos -n vamos
kubectl -n vamos port-forward svc/vamos-coordinator 18080:8080 &
./vamos build server -C examples/typescript-mcp --coordinator http://127.0.0.1:18080
helm uninstall vamos -n vamos && kubectl delete namespace vamos   # the namespace takes the volume claims
```

A worker is lost with `crictl stop --timeout 0` on its container, run in the node with `docker exec`:
the kubelet starts it again in the same pod, as Docker's restart policy does.
A worker's cgroups are seen from its own namespace, `kubectl exec <pod> -- nsenter -t 1 -C -m ls /sys/fs/cgroup/vamos`,
since `kubectl exec` enters the container's other namespaces but not that one.

## Releases

The release pipeline, its decisions and `install.sh`'s behavior are explained in
[docs/design/releases.md](../../docs/design/releases.md).
Start a release from the Actions tab, or with `gh workflow run release.yml --ref <branch> -f tag=<version>`;
the version is required, such as `0.1.0` or `0.2.0-rc.1`, and one already tagged is refused.
[Smoke check 11](smoke-checks.md) runs the release's install check on the devcontainer's platform.
The devcontainer's own builds, and the test suite, keep gpgme and btrfs, which the release binary excludes,
so a change to signature checking or storage drivers needs a release image smoke check as well.
Pull the image locally with `docker pull ghcr.io/vamos-build/vamos:<tag>`;
a cluster needs the package to be public, or a `kubernetes.io/dockerconfigjson` Secret in `imagePullSecrets`.
