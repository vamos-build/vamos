---
name: review-cycle
description: Run the review-fix loop on a commit range (HEAD by default) with the herdr `claude-reviewer` and `codex-reviewer` agents, committing accepted fixes on top until review comes back clean. Nests a fresh-session outer loop around a same-session inner loop. Use whenever the user asks to run the review cycle, get HEAD or a set of commits reviewed and fixed, or have the claude and codex reviewers look at a commit, even if they don't name this skill. Takes an optional base and an optional stacked branch to rebase afterwards.
---

# Review cycle

You are the coder. Two reviewers sit in sibling herdr tabs: `claude-reviewer` and `codex-reviewer`. Both share your checkout. You have them review a range of commits, judge their findings yourself, fix the ones you accept and commit the fixes on top. The user asked for this so they don't have to babysit the tabs. Treat it as their standing instruction to proceed on your own while each step looks as expected. Stop and ask only when something goes off track (see "Off track" below).

The cycle nests two loops:

- **Outer loop, the pass (up to 3).** Each pass starts by `/clear`ing both reviewers, then reviews the whole range from `<BASE>` to the current tip. It ends the cycle when its first round brings nothing significant that you accept.
- **Inner loop, the round (up to 7 per pass).** The reviewers keep their session across rounds. Each later round reviews only the fix commits made since the previous round. The pass ends when a round brings nothing significant that you accept.

Load the herdr skill first (`herdr --skill`) if it is not already in context. It is the authority on `herdr agent` and `herdr pane` syntax. This skill only records the workflow and the traps found while doing it.

## Why two loops

- **Inside a pass, continuity is cheap.** A reviewer that remembers its own findings can check a small fix commit against them, and can either accept your reason for declining a finding or push back with new evidence. Reviewing only the new fix commits keeps those rounds short.
- **Between passes, fresh eyes.** A session that has seen its findings answered tends to stop looking. Clearing it and reviewing the whole range again judges the code as it now stands, so an incomplete fix gets found again on its own merits. Never carry anything from an earlier pass into a fresh session, the ledger and your verdicts included, because doing so would anchor it.
- **Fixes are new commits, never amends.** The commits under review stay as they are, so each inner round can name exactly the fix commits it should look at, and every fix stays visible in `git log`.
- **You keep the memory across passes.** Your conversation and the ledger described below carry across both loops. That way you can drop a re-raised finding you already rejected without arguing it again.

## Inputs

- **Base** (optional): the commit the range starts after, taken from the user's request (for example "review everything since main"). Without one, the range is just `HEAD`, so the base is `HEAD^`.
- **Branch** (optional): a branch stacked on the commits under review that should pick up the fixes, taken from the user's request (for example "rebase remote-exec"). If no branch was named, skip the rebase rather than asking for one. Say so in your first status line ("no branch given, so no rebase") so the user can correct you early. Do not guess a branch from `git branch`.

## Preconditions

Run `test "${HERDR_ENV:-}" = 1`. If it fails, say you are not inside herdr and stop.

Keep scratch files in the session scratchpad directory, not the repo. The commands below write it as `$SCRATCH`, but shell variables do not survive between Bash calls. Put the scratchpad's absolute path in each command, or begin the command with `SCRATCH=<scratchpad path>;`. An empty `$SCRATCH` would turn `"$SCRATCH/codex.txt"` into `/codex.txt`.

## Confirm the team

Every later step sends prompts by agent name, so a wrong or missing name sends work to the wrong place or fails. Renaming, creating tabs and starting agents change the user's workspace, so offer them and wait for a yes. The standing instruction to proceed on your own starts only after the team is confirmed.

Gather the facts with read-only commands. Take your own pane, tab and workspace IDs from the live `pane current` output, not from `$HERDR_PANE_ID` and friends. Those are inherited when the pane starts and go stale if the pane is moved.

```bash
herdr pane current --current                # you: .agent kind, .pane_id, .tab_id, .workspace_id, cwd
herdr agent list                            # every live agent: name, agent (kind), pane_id, tab_id, agent_status
herdr tab list --workspace <workspace_id>   # tab labels
```

- **You** must be running `claude`. Your own name and tab label don't matter. If you are `claude-reviewer` or `codex-reviewer`, stop immediately: the user probably typed into the wrong tab, and you would end up reviewing your own fixes.
- **A reviewer is confirmed** when one live agent has the name (`claude-reviewer` or `codex-reviewer`), runs the expected kind (`claude` or `codex`), sits in a tab with the same label, and has your checkout as its cwd. A tab whose label is just its number has herdr's default label, so treat it as unnamed.

For a reviewer that is not confirmed, tell the user exactly what is wrong, then offer the smallest repair:

- **Right agent, wrong tab label:** rename the tab (`herdr tab rename <tab_id> <name>`).
- **A spare tab already runs the right kind** (not you, not the other reviewer): rename that agent and its tab. If there are several, list them with their label, cwd and status and let the user pick. Mention that the first pass will `/clear` it.
- **No candidate:** create one in your cwd. A new codex agent needs `--approve-for-me` so its review doesn't stop at a permission prompt for every tool call.

  ```bash
  herdr tab create --workspace <workspace_id> --cwd "$PWD" --label claude-reviewer --no-focus
  herdr agent start claude-reviewer --kind claude --pane <.result.root_pane.pane_id>
  herdr tab create --workspace <workspace_id> --cwd "$PWD" --label codex-reviewer --no-focus
  herdr agent start codex-reviewer --kind codex --pane <.result.root_pane.pane_id> -- --approve-for-me
  ```

- **The name runs the wrong kind, or the reviewer is in another checkout:** don't replace or move it yourself, because it may hold work the user cares about. Ask.

Check both reviewers before asking, and put every problem and offer into one message. After acting on the answers, rerun `herdr agent list` and `herdr tab list` and confirm from the fresh output. If `agent start` returns `agent_not_ready`, the new agent is blocked at a startup dialog. Read it with `herdr agent read` and ask the user rather than answering it yourself.

Both reviewers must then be `idle` (or `done`). If one is busy, inspect it with `herdr agent read` and ask. Sending a prompt into a busy agent mixes two jobs.

## Starting state

The reviewers review committed code in the shared checkout, and their sub-reviewers read surrounding files from the working tree. So the tree must be clean and the range must be the commits the user means:

```bash
git rev-parse HEAD                  # call this <REVIEWED>, the tip as the user left it
git rev-parse <base, or HEAD^>      # call this <BASE>
git merge-base --is-ancestor <BASE> HEAD && git rev-list --count <BASE>..HEAD   # must succeed and print at least 1
git log --oneline <BASE>..HEAD      # the commits under review; check they are the ones the user meant
git branch --show-current           # call this <START>; empty means detached HEAD, which is off track
git status --porcelain              # must print nothing
git for-each-ref --format='%(refname) %(objectname)' refs/heads > "$SCRATCH/refs-before.txt"
```

A range that isn't what the user meant, a detached `HEAD`, or any `status --porcelain` output is off track. Don't stash or clean it yourself, because it may be the user's work. Branch refs are shared by every worktree of the repository, so the snapshot also covers branches checked out elsewhere.

Create the ledger at `$SCRATCH/ledger.md`. Each round appends its pass and round number, the range it reviewed, and one line per finding: which reviewer(s) raised it, a short title, and your verdict (accepted, accepted but insignificant, rejected, or already addressed) with a phrase of reasoning. Note the fix commit each accepted finding landed in. The ledger lets you recognize repeats across passes, and it holds up even if your context is compacted.

## A pass (repeat up to 3 times)

### Clear both reviewers

```bash
herdr agent prompt claude-reviewer '/clear'
herdr agent prompt codex-reviewer '/clear'
```

`/clear` resets the conversation and returns to the input box immediately, so don't use `--wait`. Codex sometimes leaves `/clear` sitting in the box. Read each screen (`herdr agent read <name> --source visible --lines 40`) and press `herdr agent send-keys <name> enter` if the text is still there. Then check that the earlier conversation is gone and `herdr agent list` shows both `idle`. If `/clear` is rejected ("Unrecognized command" or similar), report it to the user rather than guessing at a substitute. Clear a stale command from the box with `ctrl+u`.

Clear only here, at the start of a pass. Never clear between rounds of a pass.

### A round (repeat up to 7 times within the pass)

#### 1. Pick the range

Always name ranges by full SHA, not by `HEAD`, because `HEAD` moves as you commit.

- **Round 1 of a pass** reviews `<BASE>..<tip>`, where `<tip>` is `git rev-parse HEAD` now. In pass 2 and later this includes the fix commits from earlier passes, which is the point: the fresh session judges the whole change as it now stands.
- **Later rounds** review `<prev>..<tip>`, where `<prev>` is the tip the previous round reviewed. These are the fix commits you made in response to that round.

Record the pass, round and range in the ledger.

#### 2. Tell the reviewers what happened (later rounds only)

Before a later round, send each reviewer a short note about its own findings from the previous round, and wait for the acknowledgement (`--wait`, then read the screen):

```text
Before your next review: since your review of <from>..<to>, commits <to>..<tip> fix these of your findings: <title> (<short sha>), ... I declined these: <title>: <one-phrase reason>, ... Reply only "noted"; the review request comes next.
```

List only that reviewer's findings, not the other reviewer's, and only from this pass. Keep the reasons to a phrase. The note is there so the reviewer can check the fixes and either accept or rebut the declines. It is not there to argue.

#### 3. Both review the range, in parallel

Start both reviews without `--wait`, so they run at the same time:

```bash
herdr agent prompt claude-reviewer '/code-review <from>..<to>'
herdr agent prompt codex-reviewer '$code-review <from>..<to>'
```

Single quotes keep the shell from expanding `$code-review`. Codex takes the `$` form and rejects `/code-review` with "Unrecognized command".

Confirm that each prompt was actually submitted. `$code-review` can open Codex's skill picker and leave the text in the box. If the box still holds the text, press Enter (`herdr agent send-keys codex-reviewer enter`). If Codex replies that the `$code-review` skill isn't installed and asks what to review, answer: `Review the commits <from>..<to> as one change, for bugs and regressions.`

Then wait for both, with a 10-minute timeout each (`herdr agent wait <name> --timeout 600000`). The two finish differently:

- **Claude's `/code-review` runs in the background**, so the agent can look idle long before the review is done. Wait for its "finished" line and the final report, not just for idle.
- **Codex fans out to five sub-reviewers**, and it can idle briefly between them. After the wait returns, confirm that the consolidated report (a `Reviewed <from>..<to>` headline followed by findings) is actually on screen.

Check that each report covers the range you asked for. A review of something else, such as an empty working-tree diff, is off track.

**Don't touch the working tree while either review runs.** Even a scratch edit inside the repo changes what the sub-reviewers read. `agent prompt` and `agent wait` can report a stall or timeout while the agent is still working fine. Read the screen before concluding anything, and never resend a review prompt without looking first.

#### 4. Collect the reports

```bash
herdr agent read claude-reviewer --source recent-unwrapped --lines 400 > "$SCRATCH/claude-p<P>r<R>.txt"
herdr agent read codex-reviewer  --source recent-unwrapped --lines 400 > "$SCRATCH/codex-p<P>r<R>.txt"
```

`recent-unwrapped` joins wrapped lines. Read the final reports only: the findings with their file and line references, the recommendations, and any design assessment. Skip the tool-call transcript and the process recap ("all five reviewers agreed…"), because a recap borrows authority the findings should earn on their own.

#### 5. Synthesize

Turn the two reports into one list of distinct findings. The reviewers often report the same issue in different words, so merge those and note that both raised it. Agreement is a reason to look closely, not proof. Then judge each finding against the code yourself:

- **Accept** it when you can confirm the defect or agree that the change improves the code. A design suggestion that would be a worthwhile change counts too.
- **Insignificant** marks an accepted finding whose fix changes no behavior and no meaningful design: wording, naming, comments, formatting, a redundant line. You may fix it, but it does not earn another round.
- **Reject** it when it is wrong, speculative without a concrete failure, out of scope for the range, or a matter of taste the codebase doesn't share. This is what "refuted" means for the loop conditions.
- **Already addressed** applies only when a branch was named and a later commit on it already fixes the problem (check with `git log`/`git show <BRANCH>`). Fixing it here as well would make the rebase conflict.
- **Repeats** are findings you rejected earlier, per the ledger. Keep the old verdict unless this round gives new reasoning or evidence, such as a rebuttal to your note, and then judge it afresh. Don't reject something just because a reviewer raised it before.

Append every verdict to the ledger.

#### 6. Fix and commit

If nothing was accepted, skip to step 7. Otherwise fix the accepted findings, then run the validation the repo requires for those changes (see `.agents/kb/testing.md`). If a fix breaks validation and you can't resolve it, go off track rather than committing broken code.

Commit the fixes on top. Never amend, and never rewrite the commits under review. Look at `git status` before staging to make sure nothing stray (a scratch file, a build output) gets swept in. One commit per round is fine. Split it when the fixes are unrelated enough that one message can't describe them. Follow the repo's message conventions, and describe what each fix changes and why, as a commit someone would write on their own. Don't mention reviewers, passes or rounds.

```bash
git add -A && git commit
git status --porcelain                      # must print nothing
git merge-base --is-ancestor <REVIEWED> HEAD   # must succeed: the reviewed commits are untouched
```

#### 7. Decide

- **Something significant was accepted:** start the next round of this pass. If this was round 7, end the pass instead. The next pass's fresh review covers the fixes.
- **Nothing significant was accepted** (everything was rejected, insignificant, or already addressed, or there were no findings): end the pass.

### After a pass

- If the pass ended in **round 1**, the fresh review found nothing significant. The cycle has converged. Go to "Finish".
- Otherwise start the next pass. If this was pass 3, go to "Finish" and say plainly that the round limit stopped the cycle before a fresh review came back clean.

Fixes for insignificant findings in the last round go unreviewed. Say so in the report.

## Finish

### Rebase, if a branch was named

The branch was built on `<REVIEWED>`, and `<START>` now has fix commits on top of it. Replay the branch's own commits onto the new tip:

```bash
git rev-parse <BRANCH>                       # call this <OLD_BRANCH> (also in refs-before.txt)
git rebase --onto <START> <REVIEWED> <BRANCH>
git switch <START>                           # leave the shared checkout where the cycle began
```

If the rebase stops on a conflict, resolve it only if the conflict comes straight from your fixes and the resolution is obvious. Otherwise, or if anything looks unclear, abort (`git rebase --abort`) and go off track.

### Verify with git

```bash
git status                                                   # clean, no rebase in progress
git merge-base --is-ancestor <REVIEWED> <START>              # the reviewed commits are untouched
git log --oneline <REVIEWED>..<START>                        # exactly the fix commits in the ledger
git range-diff <REVIEWED>..<OLD_BRANCH> <START>..<BRANCH>    # with a branch
git for-each-ref --format='%(refname) %(objectname)' refs/heads > "$SCRATCH/refs-after.txt"
diff "$SCRATCH/refs-before.txt" "$SCRATCH/refs-after.txt"
```

The range-diff must pair every old branch commit with a rebased one (`=`, or `!` where a conflict was resolved, which you should reread). A `<` line means a commit was lost. The ref diff may show only `<START>` changing, plus `<BRANCH>` when one was named. If nothing was ever accepted, nothing should have changed. Anything else is off track.

## Report

Give the user a short summary:

- **Passes and rounds:** how many of each ran, and why the cycle stopped (converged, or pass limit), and why each pass ended (clean round, or round limit).
- **Per round:** findings raised by each reviewer, which you fixed (with the commit), which you judged insignificant, and which you rejected or found already addressed, with a phrase of reasoning each. Note any decline a reviewer rebutted.
- **Commits:** the fix commits added on top of `<REVIEWED>`, by hash and subject.
- **Rebase:** the state of the rebased branch, or that no rebase was requested.

Say plainly if a step was skipped, a check was not run, or fixes went unreviewed because of the pass limit or because they were insignificant. If the pass limit was reached, offer to run another pass. Offer to squash the fix commits into the reviewed ones if the user wants a tidy history; don't do it unasked.

## Off track

Stop and tell the user what you saw when any of these happens:

- A reviewer is `blocked` on an approval or question dialog. Read it, but never answer it yourself. The one exception is the scripted Codex "skill isn't installed" reply above.
- A wait times out, or a reviewer sits `working` far past the expected time.
- A review has no recognizable final report, or reviews something other than the range you named.
- Your fixes fail validation and you can't resolve it.
- The rebase hits a conflict you can't resolve, or leaves a dirty tree.
- Any branch other than `<START>` and the named branch moves.
- At the start, the range is not the commits the user meant, `HEAD` is detached, or the tree is dirty.

Do not retry a mutating step (resending a prompt, re-running the rebase) until you have inspected the agent's screen and the git state. A timeout or stall does not prove the prompt was not delivered.
