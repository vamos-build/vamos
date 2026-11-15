# Testing gaps

The `persistent-traces` change leaves these requirements without an end-to-end scenario,
because the feature harness can't yet set them up.
Each is covered by unit tests of the logic beneath it; what is missing is the scenario that runs the real CLI.
They keep tasks 7.1, 7.4 and 8.1 of `openspec/changes/persistent-traces/tasks.md` open.

## An executor that stops answering, at each cleanup step

Task 7.1 asks that each step of a command's cleanup, run once its jobs end, stops at its bound
when the executor stops answering:
measuring the clock (5 seconds a sample), storing the command trace (10 seconds), and releasing the jobs (10 seconds).

The harness needs a coordinator, or a proxy in front of one, that holds chosen requests unanswered:
`GET /v1/capabilities` for the clock, `POST /v1/blobs` for the trace, and `POST /v1/jobs/{id}/release`.
`GivenACoordinatorThatRefusesToStoreCommandTraces` is such a proxy, which refuses rather than holds,
and could take a mode that holds instead.

Each scenario then takes at least its bound to run.
Making the bounds settable for tests, as `clockTimeout` already is within `internal/cli`,
would keep them short, but touches production code for testability.

## A prune refused while the command trace is stored

Task 7.1 asks that a prune on a coordinator is refused while a command stores its trace and assembles its file,
and runs once the command releases its jobs.

The harness needs a way to pause a command between storing its trace and releasing its jobs,
run a prune from another client in that window, check it is refused, and let the command go on.
The holding proxy above can give the pause, by holding the release until the scenario lets it through.

## An attempt that traces past the cap

Task 7.4 asks for a scenario of an attempt whose trace would be larger than `tracing.MaxTraceSize`, 16 MiB,
and is stored cut short, saying so.

Reaching the cap for real takes tens of thousands of steps, too slow for a scenario.
The harness needs a test-only way to lower the cap,
such as a variable in `internal/tracing` that a test can set, or an environment variable the worker reads,
which touches production code for testability.

## A task's cached files corrupt apart from its sources

Task 8.1's scenario "A task whose cached files are corrupt is lost" needs a task whose cached files differ from its
sources, so that corrupting them in the build cache leaves the sources intact and the next build can run the task
again, rather than failing to read the corrupt source.

A task makes such files only by running a program, and the only runnable image the scenarios use is busybox,
pulled from Docker Hub, whose pulls into a scenario's own stores risk its rate limit.
The harness needs a small runnable image in the scenario's own registry,
such as a static binary copied into an image by `GivenTheRegistryHoldsAnImage`'s machinery,
or busybox mirrored into the registry once per test run.

## Builds whose history writes run at the same moment

Task 8.1's scenario "Concurrent builds each add their action" builds one target from two clients at once, and proves
from each job's events that each job's target was building, or queued to, before the other job was done.
It doesn't prove that the two attempts ran, and wrote the target's history, at the same moment:
a job reports building when its attempt is submitted, and a target's history is written before its job stores its
trace and reports done.
The harness needs a barrier within both attempts, such as a step that waits until every attempt of the scenario has
reached it, which needs a runnable image and a way for the attempts to meet, as a file the test makes visible to both.

## An intermittent overlap in a left attempt's shared track

The scenario "A build that left a shared attempt says so" failed twice in about twenty runs,
both times under load beside the other scenarios, with the joining build's trace placing
the attempt's `run` step so that it overlaps its `env` step on the track `task "slow" (shared)`
without lying within it, which Perfetto draws wrongly.
Both failures came before the scenario waited for the other client to build its target before joining it, which
fixed a different race, in which the scenario's own build started the attempt and the other joined it; that race
put the steps on the starter's own tracks, and doesn't explain the overlap.
Its cause is unknown, and it hasn't been reproduced alone.
The scenario now also checks that the `env` step ends before the `run` step starts, in both builds' traces.
The possible causes:
the attempt's spans may be ended late when the client that started it is interrupted while they run,
or the attempt may be restarted for the job that joined it.
A scenario that reproduces it needs the interrupt to land while a given step of the attempt runs.
