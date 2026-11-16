# Building vamos with vamos

The repository root is a vamos workspace: [WORKSPACE](../../WORKSPACE) marks it,
and [BUILD](../../BUILD) builds and checks vamos with vamos.
It is a first step, not yet part of the development process:
the [Dockerfile](../../Dockerfile) still makes the release, the remote environment's images and the smoke checks' image,
and [scripts/check](../../scripts/check) still runs the checks, as [testing.md](testing.md) says.
A change to one of them needs the same change in `BUILD` until `BUILD` replaces it.

| Target | What it does | Mirrors |
| --- | --- | --- |
| `build` | The release binary: cgo, built on its target platform, with the release's tags; `--set version=v0.1.0` stamps it | The Dockerfile's `build` stage |
| `image` | The release image, on Fedora with `containers-common` and `netavark` | The Dockerfile's final stage |
| `lint`, `vulnerabilities`, `test` | Check tasks: golangci-lint, govulncheck and `go test -short ./...` | `scripts/check`, except its full tier |
| `release` | A publication of the checks and the image, to `localhost:5000/vamos` unless `--set repository=` names another; `vamos build release` runs all of them without publishing | Nothing yet |

The checks run in `check-tools`, an image of the devcontainer's environment:
Fedora's Go with the gpgme and btrfs headers that the default build tags need,
`containers-common` for the image policy that the push and publish tests read,
and golangci-lint and govulncheck at the [devcontainer](../../.devcontainer/Dockerfile)'s versions.

```sh
TMP=$(mktemp -d)
go build -o vamos . && ./vamos build release
./vamos export build "$TMP/bin" --set version=v0.0.0-test
./vamos load image vamos:self && docker run --rm vamos:self --version
```

Every example has a `WORKSPACE` of its own.
Without one, `-C examples/<name>` would find the root's `WORKSPACE` and make the example a package of this workspace,
ignoring its `vamos.lock` and resolving `//:` labels against the root `BUILD`.
A new example needs a `WORKSPACE` file as well.

## Before vamos can take itself on

- **No Go build cache.**
  Every check task compiles all its packages from scratch, since nothing persists `GOCACHE` between runs:
  a check after a source change takes about a minute rather than the devcontainer's seconds.
  Only the module download is reused, as the `modules` and `check-modules` layers.
- **No full test suite.**
  `go test ./...` builds images with buildah, which needs container storage and privileges that a `run` step lacks.
- **One platform per build.**
  cgo means `build` compiles on its target platform, so the other architecture builds under emulation;
  there is no multi-platform `index` of the release image yet.
- **No volume in the image.**
  The BUILD language can't declare `VOLUME`, so a container of `image` needs `-v /var/lib/containers`.
- **Cached vulnerability checks.**
  `vulnerabilities` reruns only when its inputs change, but the vulnerability database changes when the code doesn't:
  run it with `--rebuild`.
- **Go as cached, not as pinned.**
  `check-tools` installs whatever Go Fedora ships when the layer is built, and keeps it while the layer stays cached,
  so two machines on the same commit can check with different Go versions.
  Fedora's Go doesn't fetch a newer toolchain (`GOTOOLCHAIN=local`), so once `go.mod` needs a newer Go the checks fail
  until `vamos build check-tools --rebuild` installs a newer one,
  or, if only a later Fedora ships it, the `fedora` pull's tag is raised.
  `vamos update fedora` helps only when the image itself was republished, since the Go comes from Fedora's repositories.
