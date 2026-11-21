#!/usr/bin/env bash
# Send one round's review prompts to both reviewers, by pane, and restore
# their herdr names. Usage:
#   send-review.sh <scratch-dir> <pass> <round> <claude-pane> <codex-pane>
# Reads <scratch-dir>/prompt-claude-p<pass>r<round>.txt and
# <scratch-dir>/prompt-codex-p<pass>r<round>.txt. Sends nothing unless both
# exist and are non-empty. Does not wait for the reviews.
set -u

if [ $# -ne 5 ]; then
  echo "usage: $0 <scratch-dir> <pass> <round> <claude-pane> <codex-pane>" >&2
  exit 2
fi
scratch=$1 pass=$2 round=$3 claude_pane=$4 codex_pane=$5
claude_prompt="$scratch/prompt-claude-p${pass}r${round}.txt"
codex_prompt="$scratch/prompt-codex-p${pass}r${round}.txt"

for f in "$claude_prompt" "$codex_prompt"; do
  if [ ! -s "$f" ]; then
    echo "MISSING $f: nothing sent" >&2
    exit 1
  fi
  if [ "$(wc -l < "$f")" -gt 1 ]; then
    echo "MULTILINE $f: a newline would submit the prompt cut short; nothing sent" >&2
    exit 1
  fi
done

# Each send is independent: a stall reported for one must not keep the other
# reviewer from being prompted. A stall does not prove non-delivery, so the
# caller reads both screens before doing anything else.
status=0
herdr agent prompt "$claude_pane" "$(cat "$claude_prompt")" || status=1
herdr agent prompt "$codex_pane" "$(cat "$codex_prompt")" || status=1

# herdr drops an agent's name when /clear starts a new session (Claude's at
# once, Codex's when its next prompt starts), so put the names back for the
# user's tabs. Renaming an agent that still has its name is harmless.
herdr agent rename "$claude_pane" claude-reviewer >/dev/null || status=1
herdr agent rename "$codex_pane" codex-reviewer >/dev/null || status=1

exit $status
