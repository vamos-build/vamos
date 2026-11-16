---
name: review-cycle
description: Run the review-fix loop on a commit range (HEAD by default) with the herdr `claude-reviewer` and `codex-reviewer` agents, committing accepted fixes on top until review comes back clean. Nests a fresh-session outer loop around a same-session inner loop. Use whenever the user asks to run the review cycle, get HEAD or a set of commits reviewed and fixed, or have the claude and codex reviewers look at a commit, even if they don't name this skill. Takes an optional base and an optional stacked branch to rebase afterwards.
---

# Review cycle

You are the coder. Two reviewers sit in sibling herdr tabs: `claude-reviewer` and `codex-reviewer`. Both share your checkout. You have them review a range of commits, judge their findings yourself, fix the ones you accept and commit the fixes on top. The user asked for this so they don't have to babysit the tabs. Treat it as their standing instruction to proceed on your own while each step looks as expected. Stop and ask only when something goes off track (see "Off track" below).

The cycle nests two loops:

- **Outer loop, the pass (up to 3).** Each pass starts by `/clear`ing both reviewers, then reviews the whole range from `<BASE>` to the current tip. The cycle ends when a pass's first round brings nothing significant that you accept.
- **Inner loop, the round (up to 3 per pass).** The reviewers keep their session across rounds. Each later round reviews only the fix commits made since the previous round. The pass ends when a round brings nothing significant that you accept.

Load the herdr skill first (`herdr --skill`) if it is not already in context. It is the authority on `herdr agent` and `herdr pane` syntax. This skill only records the workflow and the traps found while doing it.

## Why two loops

- **Inside a pass, continuity is cheap.** A reviewer that remembers its own findings can check a small fix commit against them, and can either accept your reason for declining a finding or push back with new evidence.
- **Between passes, fresh eyes.** A session that has seen its findings answered tends to stop looking. Clearing it and reviewing the whole range again judges the code as it now stands, so an incomplete fix gets found again on its own merits. If fixes still draw significant findings after three rounds, a fresh pass is the better next step than a fourth round. Never carry anything from an earlier pass into a fresh session, the ledger and your verdicts included, because doing so would anchor it.
- **Fixes are new commits, never amends.** The commits under review stay as they are, so each round can name exactly the fix commits it should look at.
- **You keep the memory across passes.** Your conversation and the ledger carry across both loops, so you can drop a re-raised finding you already rejected without arguing it again.

## Inputs

- **Base** (optional): what the range starts after, taken from the user's request (for example "review everything since main"). A branch means its fork point with `HEAD`; a tag or SHA means that commit. Without one, the range is just `HEAD`.
- **Branch** (optional): a branch stacked on the commits under review that should pick up the fixes (for example "rebase remote-exec"). If none was named, skip the rebase and say so in your first status line ("no branch given, so no rebase") so the user can correct you early. Do not guess one from `git branch`.

## Preconditions

Run `test "${HERDR_ENV:-}" = 1`. If it fails, say you are not inside herdr and stop.

Keep scratch files in the session scratchpad directory, not the repo. Start each run in a new folder, `mktemp -d <scratchpad>/review-XXXX`, written `$SCRATCH` below, so an earlier run's files can't leak in. Shell variables do not survive between Bash calls, so put the absolute path in each command or begin it with `SCRATCH=<path>;` (an empty `$SCRATCH` turns `"$SCRATCH/x"` into `/x`). Name the folder in your first status line, so it survives compaction.

## Confirm the team

Renaming, creating tabs and starting agents change the user's workspace, so offer them and wait for a yes. The standing instruction to proceed on your own starts only after the team is confirmed.

Gather the facts with read-only commands. Take your own IDs from the live `pane current` output, not from `$HERDR_PANE_ID`, which goes stale if the pane is moved.

```bash
herdr pane current --current                # you: .agent kind, .pane_id, .tab_id, .workspace_id, cwd
herdr agent list                            # every live agent: name, agent (kind), pane_id, tab_id, agent_status
herdr tab list --workspace <workspace_id>   # tab labels
```

- **You** must be running `claude`. If you are `claude-reviewer` or `codex-reviewer`, stop: the user probably typed into the wrong tab, and you would end up reviewing your own fixes.
- **A reviewer is confirmed** when one live agent has the name (`claude-reviewer` or `codex-reviewer`), runs the expected kind (`claude` or `codex`), sits in a tab with the same label, and has your checkout as its cwd. A tab whose label is just its number is unnamed.

For a reviewer that is not confirmed, say exactly what is wrong and offer the smallest repair:

- **Right agent, wrong tab label:** rename the tab (`herdr tab rename <tab_id> <name>`).
- **A spare tab already runs the right kind** (not you, not the other reviewer): rename that agent and its tab. If there are several, list them with label, cwd and status and let the user pick. Mention that the first pass will `/clear` it.
- **No candidate:** create one in your cwd. A new codex agent needs `--approve-for-me` so its review doesn't stop at a permission prompt for every tool call.

  ```bash
  herdr tab create --workspace <workspace_id> --cwd "$PWD" --label claude-reviewer --no-focus
  herdr agent start claude-reviewer --kind claude --pane <.result.root_pane.pane_id>
  herdr tab create --workspace <workspace_id> --cwd "$PWD" --label codex-reviewer --no-focus
  herdr agent start codex-reviewer --kind codex --pane <.result.root_pane.pane_id> -- --approve-for-me
  ```

- **The name runs the wrong kind, or sits in another checkout:** don't replace or move it, because it may hold work the user cares about. Ask.

Put every problem and offer into one message. After acting on the answers, rerun `herdr agent list` and confirm from the fresh output. If `agent start` returns `agent_not_ready`, the agent is blocked at a startup dialog: read it with `herdr agent read` and ask the user rather than answering it yourself.

Record each reviewer's pane ID as `<CLAUDE_PANE>` and `<CODEX_PANE>`, and from here on address the reviewers by pane, not by name. `/clear` starts a new agent session and herdr then forgets the agent's name (Claude's at once, Codex's when its next prompt starts), so a name-addressed command after a clear fails or finds nothing. `scripts/send-review.sh` puts the names back after each send so the user's tabs stay labeled.

Both reviewers must be `idle` (or `done`). If one is busy, inspect it with `herdr agent read` and ask. Sending a prompt into a busy agent mixes two jobs.

## Starting state

The reviewers review committed code in the shared checkout, so the tree must be clean and the range must be the commits the user means:

```bash
git rev-parse HEAD                  # <REVIEWED>, the tip as the user left it
git merge-base '<base>' HEAD        # with a base: <BASE>. A branch gives its fork point, an ancestor commit gives itself
git rev-parse --verify 'HEAD^'      # without a base: <BASE>
git log --oneline <BASE>..HEAD      # the commits under review
git branch --show-current           # <START>; empty means detached HEAD
git status --porcelain              # must print nothing
git for-each-ref --format='%(refname) %(objectname)' refs/heads > "$SCRATCH/refs-before.txt"
```

Check the log against the user's request. It is off track if a command fails, the log is empty or not the commits the user meant (a SHA base that is not an ancestor of `HEAD` shows up here as extra commits), `HEAD` is detached, or the tree is dirty. Don't stash or clean anything, because it may be the user's work. Branch refs are shared by every worktree, so the snapshot also covers branches checked out elsewhere.

Create the ledger at `$SCRATCH/ledger.md`. Each round appends its pass and round number, the range it reviewed, and one line per finding: which reviewer(s) raised it, a short title, your verdict (accepted, insignificant, rejected, or already addressed) with a phrase of reasoning, and the fix commit for accepted ones. The ledger lets you recognize repeats across passes, and it survives compaction.

## A pass (up to 3)

### Clear both reviewers

```bash
herdr agent prompt <CLAUDE_PANE> '/clear'
herdr agent prompt <CODEX_PANE> '/clear'
```

`/clear` returns to the input box immediately, so don't use `--wait`. Codex sometimes leaves `/clear` sitting in the box: read each screen (`herdr agent read <pane> --source visible --lines 40`) and press `herdr agent send-keys <pane> enter` if it's still there. Check that the earlier conversation is gone and both are `idle`. If `/clear` is rejected ("Unrecognized command" or similar), report it rather than guessing at a substitute.

Clear only here, at the start of a pass, never between rounds.

### A round (up to 3 per pass)

#### 1. Pick the range

Name ranges by full SHA, never `HEAD`, because `HEAD` moves as you commit.

- **Round 1 of a pass** reviews `<BASE>..<tip>`, with `<tip>` from `git rev-parse HEAD` now. In later passes this includes earlier fix commits, which is the point: the fresh session judges the whole change as it now stands.
- **Later rounds** review `<prev>..<tip>`, where `<prev>` is the tip the previous round reviewed: just the fix commits made in response to it.

Record the pass, round and range in the ledger.

#### 2. Write the review prompts

Round 1 of a pass:

```text
/code-review <from>..<to> Begin the report with the line `Reviewed <from>..<to>`, as plain text.   (Claude)
$code-review <from>..<to> Begin the report with the line `Reviewed <from>..<to>`, as plain text.   (Codex)
```

Codex takes the `$` form and rejects `/code-review` with "Unrecognized command".

A later round appends to each prompt a note about that reviewer's own findings from the previous round. It goes in the review request itself, so the sub-reviewers that do the reviewing see it too:

```text
Context: these commits respond to your review of <prev-from>..<from>. They fix these of your findings: <title> (<short sha>), ... I declined these: <title>: <one-phrase reason>, ... Check the fixes, and say if a decline is wrong.
```

A reviewer with no findings last round gets `Context: these commits respond to your review of <prev-from>..<from>, which found no defects; they fix findings from the other reviewer. Review them on their own merits.` List only that reviewer's findings, only from this pass, and keep reasons to a phrase. The note lets the reviewer check fixes and rebut declines; it is not there to argue.

Write each prompt, on a single line, to a new file: `$SCRATCH/prompt-claude-p<P>r<R>.txt` and `$SCRATCH/prompt-codex-p<P>r<R>.txt`. Files avoid shell quoting trouble with apostrophes and `$`; a new name per round means a failed write can't resend an old prompt; and a newline would act as Enter and submit the prompt cut short.

#### 3. Both review the range, in parallel

```bash
.claude/skills/review-cycle/scripts/send-review.sh "$SCRATCH" <P> <R> <CLAUDE_PANE> <CODEX_PANE>
```

The script sends nothing if either file is missing or spans several lines (go off track), sends the two prompts independently without waiting, and restores the reviewers' names. A non-zero exit after sending doesn't prove a prompt wasn't delivered, so read both screens before doing anything else.

Confirm that each prompt was submitted:

- **Codex:** `$code-review` can open the skill picker and leave the text in the box. If it's still there, press Enter (`herdr agent send-keys <CODEX_PANE> enter`). If Codex says the `$code-review` skill isn't installed and asks what to review, answer `Review the commits <from>..<to> as one change, for bugs and regressions. Begin the report with the line Reviewed <from>..<to> as plain text.`, plus the `Context:` note in a later round, sent from its own file `$SCRATCH/prompt-codex-p<P>r<R>-fallback.txt`.
- **Claude:** text left in Claude's box after a review is usually Claude Code's dimmed prompt suggestion, not a draft, and a new prompt simply replaces it. Check with `herdr agent read <CLAUDE_PANE> --source visible --lines 4 --format ansi | cat -v`: `^[[2m` means dimmed. Never press Enter on it. Undimmed text is the user's own draft: save it to `$SCRATCH`, clear it with `ctrl+e ctrl+u`, and mention it in the report.

Then wait for both (`herdr agent wait <pane> --timeout 600000`):

- **Claude's `/code-review` runs in the background**, so the agent can look idle long before the review is done. Wait for its `Agent "/code-review <from>…" finished` line and the report after it.
- **Codex fans out to five sub-reviewers** and can idle briefly between them. After the wait returns, confirm the consolidated report is on screen.

**Don't touch the working tree while either review runs.** Even a scratch edit inside the repo changes what the sub-reviewers read. A stall or timeout doesn't mean the agent isn't working: read the screen before concluding anything, and never resend a review prompt without looking first.

#### 4. Collect the reports

```bash
herdr agent read <CLAUDE_PANE> --source recent-unwrapped --lines 400 > "$SCRATCH/claude-p<P>r<R>.txt"
herdr agent read <CODEX_PANE>  --source recent-unwrapped --lines 400 > "$SCRATCH/codex-p<P>r<R>.txt"
```

Within a pass the screen still holds earlier rounds, so read only this round's report: for Codex, from the `Reviewed <from>..<to>` line for this round's range; for Claude, after the `finished` line whose `<from>` matches this round's. Each report must name the range you asked for (SHA prefixes of 7+ characters count). If the report is missing, read once more with a larger `--lines`; a report that is still missing, or that reviewed a different range, is off track.

Read the findings, recommendations and any design assessment. Skip the tool-call transcript and the process recap ("all five reviewers agreed…"): a recap borrows authority the findings should earn on their own.

#### 5. Synthesize

Merge the two reports into one list of distinct findings, noting which reviewer(s) raised each. Agreement is a reason to look closely, not proof. Judge each finding against the code yourself:

- **Accept** it when you can confirm the defect or agree the change improves the code, design suggestions included.
- **Insignificant** marks an accepted finding whose fix changes no behavior and no meaningful design: wording, naming, comments, formatting, a redundant line. You may fix it, but it doesn't earn another round.
- **Reject** it when it is wrong, speculative without a concrete failure, out of scope for the range, or a matter of taste the codebase doesn't share.
- **Already addressed** applies only when a branch was named and a later commit on it already fixes the problem (check with `git log`/`git show <BRANCH>`). Fixing it here too would make the rebase conflict.
- **Repeats** of findings you rejected earlier keep the old verdict, unless this round brings new reasoning or evidence, such as a rebuttal to your note.

Append every verdict to the ledger.

#### 6. Fix and commit

If nothing was accepted, skip to step 7. Otherwise fix the accepted findings and run the validation the repo requires for those changes (see `.agents/kb/testing.md`). If you can't make validation pass, go off track rather than committing broken code.

Reviewers share the checkout and have been seen to `git checkout <sha>` mid-review, so first check `git branch --show-current` is still `<START>`. If `HEAD` is detached at `<START>`'s tip with a clean tree, `git switch <START>` and note it in the report; anything else is off track.

Make one commit for the round, on top. Never amend. Look at `git status` first so nothing stray gets swept in. Follow the repo's message conventions and describe what the fixes change and why, as a commit someone would write on their own, without mentioning reviewers, passes or rounds. Write the message to a new file so no editor opens and a failed write can't reuse an old one:

```bash
git add -A && git commit -F "$SCRATCH/commit-p<P>r<R>.txt"
git status --porcelain                         # must print nothing
git merge-base --is-ancestor <REVIEWED> HEAD   # must succeed: the reviewed commits are untouched
```

#### 7. Decide

- **Something significant was accepted:** start the next round, or, after round 3, end the pass. The next pass's fresh review covers the fixes.
- **Nothing significant was accepted** (all rejected, insignificant or already addressed, or no findings): end the pass.

### After a pass

- If the pass ended in **round 1**, the fresh review found nothing significant and the cycle has converged. Go to "Finish".
- Otherwise start the next pass. After pass 3, go to "Finish" and say plainly that the pass limit stopped the cycle before a fresh review came back clean.

## Finish

### Rebase, if a branch was named

The branch was built on `<REVIEWED>`; `<START>` now has fix commits on top of it. Replay the branch's own commits onto the new tip:

```bash
git rev-parse <BRANCH>                       # <OLD_BRANCH>
git rebase --onto <START> <REVIEWED> <BRANCH>
git switch <START>                           # leave the shared checkout where the cycle began
```

Resolve a conflict only if it comes straight from your fixes and the resolution is obvious. Otherwise `git rebase --abort` and go off track.

### Verify with git

```bash
git status                                                   # clean, no rebase in progress, on <START>
git merge-base --is-ancestor <REVIEWED> <START>              # the reviewed commits are untouched
git log --oneline <REVIEWED>..<START>                        # exactly the fix commits in the ledger
git range-diff <REVIEWED>..<OLD_BRANCH> <START>..<BRANCH>    # with a branch
git for-each-ref --format='%(refname) %(objectname)' refs/heads > "$SCRATCH/refs-after.txt"
diff "$SCRATCH/refs-before.txt" "$SCRATCH/refs-after.txt"
```

The range-diff must pair every old branch commit with a rebased one (`=`, or `!` where you resolved a conflict, which you should reread); a `<` line means a lost commit. Only `<START>`, and `<BRANCH>` if named, may have moved; if nothing was accepted, nothing should have. Anything else is off track.

## Report

Give the user a short summary:

- **Passes and rounds:** how many ran, why the cycle stopped (converged or pass limit), and why each pass ended (clean round or round limit).
- **Per round:** each reviewer's findings, which you fixed (with the commit), judged insignificant, rejected or found already addressed, with a phrase of reasoning each. Note any decline a reviewer rebutted.
- **Commits:** the fix commits on top of `<REVIEWED>`, by hash and subject.
- **Rebase:** the rebased branch's state, or that none was requested.

Say plainly if a step was skipped or a check not run, and which fixes went unreviewed (insignificant fixes in a pass's last round, or anything after the pass limit). If the pass limit was reached, offer another pass. Offer to squash the fix commits into the reviewed ones; don't do it unasked.

## Off track

Stop and tell the user what you saw when:

- A reviewer is `blocked` on an approval or question dialog. Read it, but never answer it yourself, except for the scripted Codex "skill isn't installed" reply above.
- A wait times out, or a reviewer sits `working` far past the expected time.
- A review has no recognizable report, or reviewed something other than the range you named.
- Your fixes fail validation and you can't resolve it.
- `HEAD` has left `<START>` in a way you can't safely restore.
- The rebase hits a conflict you can't resolve, or leaves a dirty tree.
- Any branch other than `<START>` and the named branch moves.
- At the start, the range is not what the user meant, `HEAD` is detached, or the tree is dirty.

Do not retry a mutating step (resending a prompt, re-running the rebase) until you have inspected the agent's screen and the git state. A timeout or stall does not prove the prompt was not delivered.
