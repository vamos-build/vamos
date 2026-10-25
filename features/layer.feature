@buildah @layer
Feature: Layers
  A layer takes an image and files in and gives an image out.
  It starts from its base's image, applies its steps in order,
  and records the runtime configuration that the image runs with.

  Background:
    Given a project that pulls busybox

  Rule: A layer's image is its base's, with the layer's changes

    Scenario: A layer keeps its base's files and adds its own
      Given the BUILD file declares:
        """
        glob(name="deps_files", include=["deps.txt"])
        layer(
            name="deps",
            base="busybox",
            steps=[add("deps_files", "/deps"), run(["sh", "-c", "echo installed > /deps/installed.txt"])],
        )
        glob(name="app_files", include=["app.txt"])
        layer(name="app", base="deps", steps=[add("app_files", "/app"), run(["cat", "/deps/installed.txt"])])
        """
      And the project holds:
        | path     | content        |
        | deps.txt | a dependency\n |
        | app.txt  | the app\n      |
      When I save "app"
      Then it succeeds
      And the output includes "installed"
      And the image holds:
        | path                | content        |
        | /deps/deps.txt      | a dependency\n |
        | /deps/installed.txt | installed\n    |
        | /app/app.txt        | the app\n      |
      And no image is left behind

    Scenario: A layer adds tasks' outputs and project files into one directory
      Given the BUILD file declares:
        ```
        glob(name="input", include=["input.txt"])
        task(
            name="shout",
            base="busybox",
            steps=[run(["sh", "-c", """
                mkdir -p /out/sub
                echo HELLO > /out/upper.txt
                echo nested > /out/sub/nested.txt
                """])],
            source="/out",
            include=["upper.txt", "sub"],
        )
        task(
            name="count",
            base="busybox",
            steps=[run(["sh", "-c", "mkdir /out && echo 6 > /out/size.txt"])],
            source="/out",
            include=["size.txt"],
        )
        layer(name="app", base="busybox", steps=[add("shout", "/app"), add("count", "/app"), add("input", "/app")])
        ```
      And the project holds:
        | path      | content |
        | input.txt | hello\n |
      When I save "app"
      Then it succeeds
      And the image's files under "/app" are:
        | path                | content  |
        | /app/upper.txt      | HELLO\n  |
        | /app/sub/nested.txt | nested\n |
        | /app/size.txt       | 6\n      |
        | /app/input.txt      | hello\n  |
      And no image is left behind

    Scenario: A layer on scratch holds only what it adds
      Given the BUILD file declares:
        """
        glob(name="hello", include=["hello.txt"])
        layer(name="app", base="scratch", steps=[add("hello", "/data")])
        """
      And the project holds:
        | path      | content       |
        | hello.txt | hello, file\n |
      When I save "app"
      Then it succeeds
      And the image's paths are:
        | path            |
        | /data           |
        | /data/hello.txt |
      And the image holds:
        | path            | content       |
        | /data/hello.txt | hello, file\n |

    Scenario: A failing step fails the layer and leaves nothing behind
      Given the BUILD file declares:
        """
        layer(name="deps", base="busybox", steps=[run(["touch", "/installed"])])
        layer(name="app", base="deps", steps=[run(["false"])])
        """
      When I save "app"
      Then it fails with:
        """
        layer "app": run(["false"]) failed
        """
      And nothing is saved
      And no image is left behind

  Rule: Steps apply in order, and later steps see earlier changes

    Scenario: Files added later replace those added before
      Given the BUILD file declares:
        """
        glob(name="conf", include=["app.conf"])
        task(
            name="generated",
            base="busybox",
            steps=[run(["sh", "-c", "mkdir /out && echo generated > /out/app.conf"])],
            source="/out",
            include=["app.conf"],
        )
        layer(name="app", base="busybox", steps=[add("conf", "/etc/app"), add("generated", "/etc/app")])
        """
      And the project holds:
        | path     | content             |
        | app.conf | from the project\n  |
      When I save "app"
      Then it succeeds
      And the image holds:
        | path              | content      |
        | /etc/app/app.conf | generated\n  |

    Scenario: env() reaches later steps and the image, build_env() only the layer's later steps
      Given the BUILD file declares:
        """
        layer(
            name="base",
            base="busybox",
            steps=[
                env(GREETING="hello"),
                build_env(TOOL="cc"),
                run(["sh", "-c", "echo $GREETING $TOOL > /during.txt"]),
            ],
        )
        layer(name="app", base="base", steps=[run(["sh", "-c", "echo $GREETING ${TOOL:-unset} > /inherited.txt"])])
        """
      When I save "app"
      Then it succeeds
      And the image holds:
        | path           | content        |
        | /during.txt    | hello cc\n     |
        | /inherited.txt | hello unset\n  |
      And the image's environment sets "GREETING" to "hello"
      And the image's environment doesn't set "TOOL"

    Scenario: workdir() makes its directory and is where later steps and the image run
      Given the BUILD file declares:
        """
        layer(
            name="app",
            base="busybox",
            steps=[workdir("/srv/app"), run(["sh", "-c", "pwd > where.txt"]), workdir("/data")],
        )
        """
      When I save "app"
      Then it succeeds
      And the image holds:
        | path              | content     |
        | /srv/app/where.txt | /srv/app\n |
      And the image has the directory "/data"
      And the image's working directory is "/data"

  Rule: Files keep their modes as a layer adds them

    Scenario: A layer adds a task's output with the modes it was selected with
      Given the BUILD file declares:
        ```
        task(
            name="make",
            base="busybox",
            steps=[run(["sh", "-c", """
                mkdir -p /out/shared
                echo tool > /out/tool
                echo map > /out/tool.map
                echo data > /out/shared/data
                chmod 4750 /out/tool
                chmod 1777 /out/shared
            """])],
            source="/out",
            include=["**"],
            exclude=["*.map"],
            mode_mask=0o7777,
        )
        task(
            name="masked",
            base="busybox",
            steps=[run(["sh", "-c", "mkdir /out && echo lib > /out/lib.so && chmod 4775 /out/lib.so"])],
            source="/out",
            include=["*.so"],
        )
        layer(name="app", base="busybox", steps=[add("make", "/app"), add("masked", "/app/lib")])
        ```
      When I save "app"
      Then it succeeds
      And the image's files under "/app" are:
        | path             | content | mode |
        | /app/tool        | tool\n  | 4750 |
        | /app/shared/data | data\n  | 0644 |
        | /app/lib/lib.so  | lib\n   | 0755 |
      And the image has the directory "/app/shared" with mode 1777

  Rule: A layer's command and entrypoint work as a Dockerfile's CMD and ENTRYPOINT

    Scenario Outline: <case>
      Given the BUILD file declares:
        """
        layer(name="deps", base="busybox", command=["echo", "from deps"])
        layer(name="server", base="busybox", entrypoint=["java", "-jar", "server.jar"])
        layer(name="app", base="<base>", <arguments>)
        """
      When I save "app"
      Then it succeeds
      And the image's entrypoint is <entrypoint>
      And the image's command is <command>

      Examples:
        | case                                          | base    | arguments                              | entrypoint                     | command               |
        | A layer keeps its base's command              | busybox |                                        | none                           | ["sh"]                |
        | A layer keeps its built base's command        | deps    |                                        | none                           | ["echo", "from deps"] |
        | A command replaces its base's                 | busybox | command=["echo", "hi"]                 | none                           | ["echo", "hi"]        |
        | An entrypoint drops its base's command        | busybox | entrypoint=["java", "-jar", "app.jar"] | ["java", "-jar", "app.jar"]    | none                  |
        | A command follows the entrypoint as arguments | busybox | entrypoint=["echo"], command=["hello"] | ["echo"]                       | ["hello"]             |
        | A layer keeps its base's entrypoint           | server  | command=["--port", "9"]                | ["java", "-jar", "server.jar"] | ["--port", "9"]       |
        | An empty entrypoint removes its base's        | server  | entrypoint=[]                          | none                           | none                  |
