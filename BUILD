# vamos builds itself: the release image's targets mirror the Dockerfile's stages,
# and the check tasks run what scripts/check runs, but the full test suite, which needs container storage.
# The Dockerfile and scripts/check remain what the release and contributors use;
# see .agents/kb/self-build.md for what is still missing before this file can replace them.
# Each example beneath has a WORKSPACE of its own, so it stays a workspace apart from this one.
#
#   vamos build release    every check, then the release image
#   vamos build lint       one check, or vulnerabilities or test

# Debian 12's glibc is old enough for the binary to run on Ubuntu 24.04 and later: it needs glibc 2.34.
pull(
    name = "golang",
    image = "docker.io/library/golang",
    tag = "1.26-bookworm",
)

pull(
    name = "fedora",
    image = "docker.io/library/fedora",
    tag = "44",
)

glob(
    name = "module-files",
    include = [
        "go.mod",
        "go.sum",
    ],
)

glob(
    name = "source-files",
    include = [
        "go.mod",
        "go.sum",
        "main.go",
        "internal",
    ],
    exclude = [
        "**/*_test.go",
        "**/testdata",
        "internal/features",
    ],
)

# The downloaded modules, kept as an image so a source change doesn't fetch them again.
layer(
    name = "modules",
    base = "golang",
    steps = [
        workdir("/build"),
        add("module-files", "/build"),
        run(["go", "mod", "download"]),
    ],
)

# Buildah's namespace setup needs cgo, so the binary can't be static and is built on its target platform;
# these tags drop its other C libraries: signatures are checked in Go, not by gpgme, and there is no btrfs storage.
# A release sets version, such as v0.1.0, which `vamos --version` reports.
task(
    name = "build",
    params = {"version": ""},
    base = "modules",
    steps = lambda ctx: [
        add("source-files", "/build"),
        run([
            "go",
            "build",
            "-tags",
            "containers_image_openpgp exclude_graphdriver_btrfs",
            "-ldflags",
            "-X github.com/vamos-build/vamos/internal/run.stampedVersion=" + ctx.version,
            "-o",
            "vamos",
            ".",
        ]),
    ],
    source = "/build",
    include = ["vamos"],
)

# The release image: build steps run with chroot isolation, so no OCI runtime is needed.
# Container storage must not live on the container's own overlay filesystem: mount a volume at /var/lib/containers.
layer(
    name = "image",
    params = {"version": ""},
    base = "fedora",
    steps = [
        run([
            "dnf",
            "-y",
            "install",
            "containers-common",
            "netavark",
        ]),
        run(["dnf", "clean", "all"]),
        env(BUILDAH_ISOLATION = "chroot"),
        add(dep("build", version = arg("version")), "/usr/local/bin"),
        workdir("/build"),
    ],
    entrypoint = ["vamos"],
)

# The checks run in the devcontainer's environment: Fedora's Go, with the C headers that the default build tags need,
# since the devcontainer's builds keep gpgme and btrfs, which the release binary drops.
# Keep the tools' versions with the devcontainer's.
layer(
    name = "check-tools",
    base = "fedora",
    steps = [
        run([
            "dnf",
            "-y",
            "install",
            "btrfs-progs-devel",
            "containers-common",  # the image policy that the push and publish tests read
            "golang",
            "gpgme-devel",
        ]),
        run(["dnf", "clean", "all"]),
        build_env(GOBIN = "/usr/local/bin"),
        # golangci-lint built by Fedora's Go, which is go.mod's, so that it can load the packages.
        run(["go", "install", "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0"]),
        run(["go", "install", "golang.org/x/vuln/cmd/govulncheck@v1.9.0"]),
        run(["sh", "-c", "rm -rf \"$(go env GOMODCACHE)\" \"$(go env GOCACHE)\""]),
    ],
)

layer(
    name = "check-modules",
    base = "check-tools",
    steps = [
        workdir("/workspace"),
        add("module-files", "/workspace"),
        run(["go", "mod", "download"]),
    ],
)

# Everything the checks read: the tests load the examples' BUILD files too, but none of their dependencies.
glob(
    name = "check-files",
    include = [
        ".golangci.yml",
        "doc.go",
        "go.mod",
        "go.sum",
        "main.go",
        "internal",
        "examples",
    ],
    exclude = [
        "**/node_modules",
        "**/.venv",
    ],
)

layer(
    name = "check-source",
    base = "check-modules",
    steps = [add("check-files", "/workspace")],
)

task(
    name = "lint",
    base = "check-source",
    steps = [
        run(["golangci-lint", "run"]),
    ],
)

# Fails only for a vulnerability that vamos's code reaches; it needs the network, for vuln.go.dev,
# and finds a new vulnerability only once its inputs change or it runs with --rebuild.
task(
    name = "vulnerabilities",
    base = "check-source",
    steps = [
        run(["govulncheck", "./..."]),
    ],
)

task(
    name = "test",
    base = "check-source",
    steps = [
        run(["go", "test", "-short", "./..."]),
    ],
)

# The checks and the release image. `vamos build release` runs them all without publishing;
# the release itself still builds and publishes the multi-platform image from the Dockerfile.
# `repository` chooses where the image goes. Its default is a local registry, to try it,
# not the release's repository: this single-platform image without a volume must not replace the release's image.
publication(
    name = "release",
    params = {
        "repository": "localhost:5000/vamos",
        "version": "",
    },
    checks = [
        "lint",
        "vulnerabilities",
        "test",
    ],
    images = lambda ctx: {
        ctx.repository: dep("image", version = ctx.version),
    },
)
