<h1 align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/site/src/assets/logo-dark.svg">
    <img alt="vamos" src="docs/site/src/assets/logo-light.svg" width="300">
  </picture>
</h1>

<p align="center">The lovechild of Bazel and Buildah.</p>

Starlark rules to build containers and other artifacts.
A Go CLI that reads `BUILD` files, written in [Starlark](https://github.com/bazelbuild/starlark),
that describe the steps producing container images and filesets,
then runs those steps with the [buildah](https://github.com/containers/buildah) library.

Start with the [fundamentals](https://vamos.build/fundamentals/) to learn images, filesets, layers and tasks,
and follow a website's `BUILD` file from source into a running container.
To build your own project, [install Vamos](https://vamos.build/install/) in its devcontainer,
or on a Linux machine, with a release's `install.sh`.
The [user guide](https://vamos.build/) links to the CLI, BUILD language, configuration, and cache references.

## Development

Use the repository's [devcontainer](.devcontainer/devcontainer.json) for build dependencies and container privileges.
See the [design documents](docs/design/README.md) for how Vamos is built and why,
and the [contributor index](AGENTS.md) for development and validation instructions.

Build the CLI image from the repository root:

```sh
docker build . -t vamos
```

## Test

```sh
go test ./...          # everything, including image builds; needs root
go test -short ./...   # command line and BUILD file checks only
```

Image builds use `busybox` from Docker Hub.
`push` and `publish` are tested against an in-process registry and `load` against a fake Docker daemon;
if a real Docker daemon is running, `load` is also checked against it.
The tests use the machine's build cache, and one of them empties it,
so the next build of your own project runs every target again.

## License

Vamos is released under the [MIT License](LICENSE).
