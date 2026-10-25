@buildah @task
Feature: Tasks
  A task takes images and files in and gives files out.
  It runs its steps in a container made from its base,
  keeps only the files it selects beneath its source, and discards the container.
  A task without output is a check: it succeeds or fails.

  Background:
    Given a project that pulls busybox

  Rule: A task's output is the files it selects beneath its source

    Scenario: Only selected files are output, at their paths relative to the source
      Given the BUILD file declares:
        ```
        glob(name="input", include=["input.txt"])
        task(
            name="shout",
            base="busybox",
            steps=[
                add("input", "/work"),
                run(["sh", "-c", """
                mkdir -p /work/out/sub
                tr a-z A-Z < /work/input.txt > /work/out/upper.txt
                echo nested > /work/out/sub/nested.txt
                echo left out > /work/out/skipped.txt
                echo shouting
                """]),
            ],
            source="/work/out",
            include=["upper.txt", "sub"],
        )
        ```
      And the project holds:
        | path      | content |
        | input.txt | hello\n |
      When I export "shout"
      Then it succeeds
      And the output includes "shouting"
      And the exported files are:
        | path           | content  |
        | upper.txt      | HELLO\n  |
        | sub/nested.txt | nested\n |

    Scenario: Output keeps its modes, within its mode mask
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
        ```
      When I export "make"
      Then it succeeds
      And the exported files are:
        | path        | content | mode |
        | tool        | tool\n  | 4750 |
        | shared/data | data\n  | 0644 |
      And the exported folder has the directory "shared" with mode 1777

    Scenario: The default mode mask retains only the permission bits of 0o755
      Given the BUILD file declares:
        """
        task(
            name="make",
            base="busybox",
            steps=[run(["sh", "-c", "mkdir /out && echo lib > /out/lib.so && chmod 4777 /out/lib.so"])],
            source="/out",
            include=["*.so"],
        )
        """
      When I export "make"
      Then it succeeds
      And the exported files are:
        | path   | content | mode |
        | lib.so | lib\n   | 0755 |

  Rule: A task's container is discarded

    Scenario: A layer that adds a task's output gets that output alone
      Given the BUILD file declares:
        """
        task(
            name="make",
            base="busybox",
            steps=[run(["sh", "-c", "mkdir -p /work/out && echo made > /work/out/made.txt && touch /work/scratch"])],
            source="/work/out",
            include=["made.txt"],
        )
        layer(name="app", base="busybox", steps=[add("make", "/app")])
        """
      When I save "app"
      Then it succeeds
      And the image's files under "/app" are:
        | path          | content |
        | /app/made.txt | made\n  |
      And the image holds nothing under "/work"
      And no image is left behind

  Rule: A task takes images and files in

    Scenario: A task runs with its base's working directory and environment
      Given the BUILD file declares:
        """
        layer(name="tools", base="busybox", steps=[env(GREETING="hello"), workdir("/srv")])
        task(
            name="greet",
            base="tools",
            steps=[run(["sh", "-c", "mkdir out && echo $GREETING from $(pwd) > out/greeting.txt"])],
            source="/srv/out",
            include=["greeting.txt"],
        )
        """
      When I export "greet"
      Then it succeeds
      And the exported files are:
        | path         | content           |
        | greeting.txt | hello from /srv\n |

    Scenario: A task can add project files and another task's output
      Given the BUILD file declares:
        """
        glob(name="input", include=["input.txt"])
        task(
            name="shout",
            base="busybox",
            steps=[add("input", "/in"), run(["sh", "-c", "mkdir /out && tr a-z A-Z < /in/input.txt > /out/upper.txt"])],
            source="/out",
            include=["upper.txt"],
        )
        task(
            name="count",
            base="busybox",
            steps=[add("shout", "/count"), run(["sh", "-c", "wc -c < /count/upper.txt > /count/size.txt"])],
            source="/count",
            include=["size.txt"],
        )
        """
      And the project holds:
        | path      | content |
        | input.txt | hello\n |
      When I export "count"
      Then it succeeds
      And the exported files are:
        | path     | content |
        | size.txt | 6\n     |

  Rule: A task without output is a check

    Scenario: A check runs, and is reused while its inputs are unchanged
      Given the BUILD file declares:
        """
        layer(name="app", base="busybox")
        task(name="test", base="app", steps=[run(["echo", "tested {{nonce}}"])])
        """
      When I build "test"
      Then it succeeds
      And task "test" ran
      And the output includes "tested {{nonce}}"
      When I build "test" again
      Then it succeeds
      And task "test" was reused from the cache
      And the output includes "tested {{nonce}}"
      And no image is left behind

    Scenario: A failing check fails the build
      Given the BUILD file declares:
        """
        layer(name="app", base="busybox")
        task(name="broken", base="app", steps=[run(["false"])])
        """
      When I build "broken"
      Then it fails with:
        """
        task "broken": run(["false"]) failed
        """
      And no image is left behind

  Rule: A task whose output can't be selected fails, leaving nothing behind

    Scenario Outline: <case>
      Given the BUILD file declares:
        """
        task(name="make", base="busybox", steps=[run(<command>)], source="<source>", include=["<include>"])
        """
      When I export "make"
      Then it fails with:
        """
        <error>
        """
      And nothing is exported
      And no image is left behind

      Examples:
        | case                     | command                                                     | source   | include    | error                                                          |
        | The command fails        | ["false"]                                                   | /        | bin        | task "make": run(["false"]) failed                             |
        | The source is missing    | ["true"]                                                    | /nope    | app        | task "make": source "/nope" not found                          |
        | The source is a file     | ["true"]                                                    | /bin/sh  | app        | task "make": source "/bin/sh" is not a directory               |
        | An include is missing    | ["mkdir", "/out"]                                           | /out     | app.js     | task "make": include "app.js" not found in /out                |
        | An include is a link     | ["sh", "-c", "mkdir /out && ln -s /etc/passwd /out/passwd"] | /out     | passwd     | task "make": include "passwd" is a symbolic link               |
        | An include's parent is a link | ["sh", "-c", "mkdir /out && ln -s /etc /out/etc"]      | /out     | etc/passwd | task "make": include "etc/passwd" has symbolic link parent "/out/etc" |

    Scenario: A layer that adds a failing task's output fails too
      Given the BUILD file declares:
        """
        task(name="make", base="busybox", steps=[run(["false"])], source="/", include=["bin"])
        layer(name="app", base="busybox", steps=[add("make", "/app")])
        """
      When I save "app"
      Then it fails with:
        """
        task "make": run(["false"]) failed
        exit status 1
        """
      And nothing is saved
      And no image is left behind

    Scenario Outline: <case>
      Given the BUILD file declares:
        """
        task(name="make", base="busybox", steps=[run(<command>)], source="<source>", include=["<include>"])
        layer(name="app", base="busybox", steps=[add("make", "/app")])
        """
      When I save "app"
      Then it fails with:
        """
        <error>
        """
      And nothing is saved
      And no image is left behind

      Examples:
        | case                                        | command                                                     | source  | include    | error                                                                 |
        | A layer can't add a missing source          | ["true"]                                                    | /nope   | app        | task "make": source "/nope" not found                                 |
        | A layer can't add a source that is a file   | ["true"]                                                    | /bin/sh | app        | task "make": source "/bin/sh" is not a directory                      |
        | A layer can't add a missing include         | ["mkdir", "/out"]                                           | /out    | app.js     | task "make": include "app.js" not found in /out                       |
        | A layer can't add an include that is a link | ["sh", "-c", "mkdir /out && ln -s /etc/passwd /out/passwd"] | /out    | passwd     | task "make": include "passwd" is a symbolic link                      |
        | A layer can't add an include under a link   | ["sh", "-c", "mkdir /out && ln -s /etc /out/etc"]           | /out    | etc/passwd | task "make": include "etc/passwd" has symbolic link parent "/out/etc" |
