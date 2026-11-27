# Vamos builds itself: the release image's targets mirror the Dockerfile's stages,
# and the check tasks run what scripts/check runs, but the full test suite, which needs container storage.
# The Dockerfile and scripts/check remain what the release and contributors use;
# see .agents/kb/self-build.md for what is still missing before this file can replace them.
# Each example beneath has a WORKSPACE of its own, so it stays a workspace apart from this one.
#
#   vamos build release    every check, then the release image
#   vamos build lint       one check, or vulnerabilities or test
#
# The Go commands mount a tool cache, so they need chroot isolation, as the devcontainer's BUILDAH_ISOLATION sets.

# Go's build cache, which the Go commands below keep between builds, so that a source change compiles only what it
# touches. Nothing in any image is at its path.
tool_cache(
    name = "go-build",
    path = "/cache/go-build",
)

# go runs a Go command, argv, through sh, with GOCACHE, and golangci-lint's GOLANGCI_LINT_CACHE, beneath the go-build
# cache in a directory named by a digest of the image's native build environment: its packages, by rpm, and by dpkg
# with their state, and what /usr/local holds of tools, such as golangci-lint, and C headers, libraries and pkg-config
# files, in bin, include, lib, lib64 and share/pkgconfig: every entry, through links, by its name and type, each link
# by its target and each file by its contents.
# Go's build cache misses a change to the C libraries and headers that cgo uses, so a base that installs other ones
# compiles into an empty directory rather than reuse what was built against the old ones. Every step writes a file it
# checks, so that a failure to name the environment, or a base that lists no packages, fails the step rather than
# choose a cache by part of it.
# argv reaches the command unchanged, as "$@", with sh as $0.
def go(argv):
    script = r"""
        set -e
        dir=$(mktemp -d)
        : >"$dir/packages"
        if command -v rpm >/dev/null; then
            rpm -qa --qf '%{NAME}-%{EPOCHNUM}:%{VERSION}-%{RELEASE}.%{ARCH}\n' >>"$dir/packages"
        fi
        if command -v dpkg-query >/dev/null; then
            dpkg-query -W -f '${db:Status-Abbrev} ${Package}:${Architecture} ${Version}\n' >>"$dir/packages"
        fi
        if [ ! -s "$dir/packages" ]; then
            echo "go(): neither rpm nor dpkg-query lists the image's packages, which name Go's build cache" >&2
            exit 1
        fi
        LC_ALL=C sort "$dir/packages" >"$dir/list"
        roots=""
        for root in /usr/local/bin /usr/local/include /usr/local/lib /usr/local/lib64 /usr/local/share/pkgconfig; do
            if [ -e "$root" ] || [ -L "$root" ]; then roots="$roots $root"; fi
        done
        if [ -n "$roots" ]; then
            find -L $roots -print0 >"$dir/entries"
            LC_ALL=C sort -z "$dir/entries" >"$dir/sorted"
            LC_ALL=C QUOTING_STYLE=shell-escape-always xargs -0 stat --printf '%F %N\0' <"$dir/sorted" >>"$dir/list"
            find -L $roots -type f -print0 >"$dir/files"
            LC_ALL=C sort -z "$dir/files" >"$dir/sorted"
            if [ -s "$dir/sorted" ]; then xargs -0 sha256sum <"$dir/sorted" >>"$dir/list"; fi
        fi
        sha256sum <"$dir/list" >"$dir/sum"
        key=$(cut -c1-32 "$dir/sum")
        rm -rf "$dir"
        if [ "${#key}" -ne 32 ]; then echo "go(): no digest of the environment, $key" >&2; exit 1; fi
        export GOCACHE=/cache/go-build/$key/go GOLANGCI_LINT_CACHE=/cache/go-build/$key/golangci-lint
        exec "$@"
    """
    return run(["sh", "-c", script, "sh"] + argv)

# Debian 12's glibc is old enough for the binary to run on Ubuntu 24.04 and later: it needs glibc 2.34.
pull(
    name = "golang",
    image = "docker.io/library/golang",
    tag = "1.27-bookworm",
)

pull(
    name = "fedora",
    image = "docker.io/library/fedora",
    tag = "45",
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
    tool_caches = [mount("go-build")],
    steps = lambda ctx: [
        add("source-files", "/build"),
        go([
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
    tool_caches = [mount("go-build")],
    steps = [
        go(["golangci-lint", "run"]),
    ],
)

# Fails only for a vulnerability that Vamos's code reaches; it needs the network, for vuln.go.dev,
# and finds a new vulnerability only once its inputs change or it runs with --rebuild.
task(
    name = "vulnerabilities",
    base = "check-source",
    tool_caches = [mount("go-build")],
    steps = [
        go(["govulncheck", "./..."]),
    ],
)

# -count=1, since go test reuses a cached result while the files a test reads keep their size and modification time,
# and Vamos gives every file it adds the same time; the build cache is still used.
task(
    name = "test",
    base = "check-source",
    tool_caches = [mount("go-build")],
    steps = [
        go(["go", "test", "-short", "-count=1", "./..."]),
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
