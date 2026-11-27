# Building Vamos with Vamos

The repository root is a Vamos workspace: [WORKSPACE](../../WORKSPACE) marks it,
and [BUILD](../../BUILD) builds and checks Vamos with Vamos.
It is a first step, not yet part of the development process:
the [Dockerfile](../../Dockerfile) still makes the release, the remote environment's images and the smoke checks' image,
and [scripts/check](../../scripts/check) still runs the checks, as [testing.md](testing.md) says.
A change to one of them needs the same change in `BUILD` until `BUILD` replaces it.

| Target | What it does | Mirrors |
| --- | --- | --- |
| `build` | The release binary: cgo, built on its target platform, with the release's tags; `--set version=v0.1.0` stamps it | The Dockerfile's `build` stage |
| `image` | The release image, on Fedora with `containers-common` and `netavark` | The Dockerfile's final stage |
| `lint`, `markdown`, `vulnerabilities`, `test` | Check tasks: golangci-lint, markdownlint-cli2, govulncheck and `go test -short -count=1 ./...` | `scripts/check`, except its full tier; `-count=1` because Vamos gives every added file the same time |
| `release` | A publication of the checks and the image, to `localhost:5000/vamos` unless `--set repository=` names another; `vamos build release` runs all of them without publishing | Nothing yet |

The checks run in `check-tools`, an image of the devcontainer's environment:
Fedora's Go with the gpgme and btrfs headers that the default build tags need,
`containers-common` for the image policy that the push and publish tests read,
and golangci-lint and govulncheck at the [devcontainer](../../.devcontainer/Dockerfile)'s versions.
`markdown` runs in `markdown-tools` instead, Fedora's Node.js with markdownlint-cli2 at the devcontainer's version,
so that Node.js stays out of the environment that names the Go checks' cache.

The workspace is named `build.vamos` in [WORKSPACE](../../WORKSPACE),
so that `build`, `lint`, `vulnerabilities` and `test` can mount
the [tool cache](../../docs/user/reference/build/tool-cache.md) `go-build` at `/cache/go-build`
and keep Go's build cache, and golangci-lint's, between builds:
a check after a one-file change takes seconds, where it used to compile everything.
The Go commands run through `go()` in `BUILD`, which sets `GOCACHE` and `GOLANGCI_LINT_CACHE` beneath the mount,
in a directory named by a digest of the image's installed packages, by rpm and dpkg,
and of what `/usr/local` holds of tools, such as golangci-lint, and C headers, libraries and pkg-config files,
in `bin`, `include`, `lib`, `lib64` and `share/pkgconfig`,
since Go's build cache misses a change to the C libraries and headers that cgo uses;
a base that lists no packages, or whose environment can't be read, fails the step rather than choose a cache.
The digest misses a package's file changed in place without a new package version,
a file elsewhere than those directories, a change in where a link outside them leads,
and compiler settings such as `CPATH` and `PKG_CONFIG_PATH`:
after a base makes such a change, a standalone `vamos prune` clears the cache it would otherwise reuse.
`test` passes `-count=1`, since `go test` would reuse a cached result for files whose size and time didn't change,
and Vamos gives every file it adds the same time.
`vamos build lint --no-tool-caches --rebuild` checks from an empty cache.
A cache of an environment that changed, as a rebuilt `check-tools` may, is left beside the new one
until a standalone `vamos prune` clears them all; a local coordinator's prune leaves them.

These commands run in the devcontainer, whose `BUILDAH_ISOLATION=chroot` tool caches need:

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

## Before Vamos can take itself on

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
