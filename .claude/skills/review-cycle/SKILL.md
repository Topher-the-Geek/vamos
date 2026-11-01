---
name: review-cycle
description: Run the review-fix loop on HEAD with the herdr `claude-reviewer` and `codex-reviewer` agents, amending accepted fixes into the commit until review comes back clean. Use whenever the user asks to run the review cycle, get HEAD reviewed and fixed, or have the claude and codex reviewers look at a commit, even if they don't name this skill. Takes an optional stacked branch to rebase afterwards.
---

# Review cycle

You are the coder. Two reviewers sit in sibling herdr tabs: `claude-reviewer` and `codex-reviewer`. Both share your checkout. Each round you clear both reviewers, have them review `HEAD`, judge their findings yourself, fix the ones you accept and amend `HEAD`. The loop ends when a round brings no findings you accept, or after 3 rounds. The user asked for this so they don't have to babysit the tabs. Treat it as their standing instruction to proceed on your own while each step looks as expected. Stop and ask only when something goes off track (see "Off track" below).

Load the herdr skill first (`herdr --skill`) if it is not already in context. It is the authority on `herdr agent` and `herdr pane` syntax. This skill only records the workflow and the traps found while doing it.

## Why amend, and why the reviewers start fresh

Every round amends the fixes into the single commit under review, and both reviewers are `/clear`ed before they review it again. Two reasons:

- **The reviewers judge the code, not its history.** A fresh reviewer that sees only `HEAD` has no "fix review findings" commits to tell it what the last round worried about. A fix that is incomplete gets found again on its own merits, and the last round reviews exactly the commit that ships. Both review skills are written for `HEAD`, so this needs no range arguments.
- **You keep the memory instead.** Your conversation carries across rounds, together with the ledger described below. That way you can drop a re-raised finding you already rejected without arguing it again, and you can still see what each round changed. Never send the ledger or your earlier verdicts to the reviewers, because doing so would anchor them.

## Inputs

- **Branch** (optional): a branch stacked on the commit under review that should pick up the fixes, taken from the user's request (for example "rebase remote-exec"). If no branch was named, skip the rebase rather than asking for one. Say so in your first status line ("no branch given, so no rebase") so the user can correct you early. Do not guess a branch from `git branch`.

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
- **A spare tab already runs the right kind** (not you, not the other reviewer): rename that agent and its tab. If there are several, list them with their label, cwd and status and let the user pick. Mention that the first round will `/clear` it.
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

The reviewers review `HEAD` of the shared checkout, and their sub-reviewers read surrounding files from the working tree. Codex reviews uncommitted changes instead of `HEAD` when there are any. So the tree must be clean and `HEAD` must be the commit the user means:

```bash
git rev-parse HEAD                  # call this <REVIEWED>
git rev-parse HEAD^                 # call this <BASE>; the cycle keeps exactly one commit on top of it
git branch --show-current           # call this <START>; empty means detached HEAD, which is off track
git status --porcelain              # must print nothing
git for-each-ref --format='%(refname) %(objectname)' refs/heads > "$SCRATCH/refs-before.txt"
```

A `HEAD` that isn't the commit the user meant, a detached `HEAD`, or any `status --porcelain` output is off track. Don't stash or clean it yourself, because it may be the user's work. Branch refs are shared by every worktree of the repository, so the snapshot also covers branches checked out elsewhere.

Create the ledger at `$SCRATCH/ledger.md`. Each round appends the `HEAD` SHA it reviewed and one line per finding: which reviewer(s) raised it, a short title, and your verdict (accepted, rejected, or already addressed) with a phrase of reasoning. The ledger lets you recognize repeats and recover from a bad fix, and it holds up even if your context is compacted. Old round SHAs also stay reachable through the reflog, so `git diff <round-N sha> HEAD` shows what any round changed.

## The round (repeat up to 3 times)

### 1. Clear both reviewers

```bash
herdr agent prompt claude-reviewer '/clear'
herdr agent prompt codex-reviewer '/clear'
```

`/clear` resets the conversation and returns to the input box immediately, so don't use `--wait`. Codex sometimes leaves `/clear` sitting in the box. Read each screen (`herdr agent read <name> --source visible --lines 40`) and press `herdr agent send-keys <name> enter` if the text is still there. Then check that the earlier conversation is gone and `herdr agent list` shows both `idle`. If `/clear` is rejected ("Unrecognized command" or similar), report it to the user rather than guessing at a substitute. Clear a stale command from the box with `ctrl+u`.

### 2. Both review HEAD, in parallel

Record the round first (`git rev-parse HEAD` into the ledger). Then start both reviews without `--wait`, so they run at the same time:

```bash
herdr agent prompt claude-reviewer '/code-review HEAD'
herdr agent prompt codex-reviewer '$code-review'
```

Single quotes keep the shell from expanding `$code-review`. Codex takes the `$` form and rejects `/code-review` with "Unrecognized command".

Confirm that each prompt was actually submitted. `$code-review` can open Codex's skill picker and leave the text in the box. If the box still holds the text, press Enter (`herdr agent send-keys codex-reviewer enter`). If Codex replies that the `$code-review` skill isn't installed and asks what to review, answer: `Review the most recent commit, HEAD (<sha>), for bugs and regressions.`

Then wait for both, with a 10-minute timeout each (`herdr agent wait <name> --timeout 600000`). The two finish differently:

- **Claude's `/code-review` runs in the background**, so the agent can look idle long before the review is done. Wait for its "finished" line and the final report, not just for idle.
- **Codex fans out to five sub-reviewers**, and it can idle briefly between them. After the wait returns, confirm that the consolidated report (a "Reviewed HEAD <sha>" style headline followed by findings) is actually on screen.

**Don't touch the working tree while either review runs.** Even a scratch edit inside the repo would change what Codex reviews. `agent prompt` and `agent wait` can report a stall or timeout while the agent is still working fine. Read the screen before concluding anything, and never resend a review prompt without looking first.

### 3. Collect the reports

```bash
herdr agent read claude-reviewer --source recent-unwrapped --lines 400 > "$SCRATCH/claude-round<N>.txt"
herdr agent read codex-reviewer  --source recent-unwrapped --lines 400 > "$SCRATCH/codex-round<N>.txt"
```

`recent-unwrapped` joins wrapped lines. Read the final reports only: the findings with their file and line references, the recommendations, and any design assessment. Skip the tool-call transcript and the process recap ("all five reviewers agreed…"), because a recap borrows authority the findings should earn on their own.

### 4. Synthesize

Turn the two reports into one list of distinct findings. The reviewers often report the same issue in different words, so merge those and note that both raised it. Agreement is a reason to look closely, not proof. Then judge each finding against the code yourself:

- **Accept** it when you can confirm the defect or agree that the change improves the commit. A design suggestion that would be a worthwhile change counts too.
- **Reject** it when it is wrong, speculative without a concrete failure, out of scope for this commit, or a matter of taste the codebase doesn't share.
- **Already addressed** applies only when a branch was named and a later commit on it already fixes the problem (check with `git log`/`git show <BRANCH>`). Fixing it here as well would make the rebase conflict.
- **Repeats** are findings you rejected in an earlier round, per the ledger. Keep the old verdict unless this round gives new reasoning or evidence, and then judge it afresh. Don't reject something just because a reviewer raised it before.

Append every verdict to the ledger. If nothing was accepted, the cycle has converged. Skip to "Finish". Do the same if both reviews came back with no findings at all.

### 5. Fix and amend

Fix the accepted findings, then run the validation the repo requires for those changes (see `.agents/kb/testing.md`). If a fix breaks validation and you can't resolve it, go off track rather than amending a broken commit.

Amend the fixes into the commit. The tree was clean when the cycle started, so every change in it is yours. Still, look at `git status` before staging to make sure nothing stray (a scratch file, a build output) gets swept in. If the fixes change what the commit does, rewrite the message so it describes the commit as it now stands, following the repo's conventions. Don't mention the review rounds, because the message should read as if the commit had been written right the first time.

```bash
git add -A && git commit --amend            # or --amend --no-edit if the message still fits
git status --porcelain                      # must print nothing
git rev-parse HEAD^                         # must still equal <BASE>
```

Then go on to the next round. If this was round 3, the fixes you just amended have not been reviewed. Go to "Finish" and say so plainly.

## Finish

### Rebase, if a branch was named

The branch was built on the old `<REVIEWED>` commit, which the amends rewrote. A plain `git rebase` would replay the old commit on top of the new one and conflict. Cut it off with `--onto`:

```bash
git rev-parse <BRANCH>                       # call this <OLD_BRANCH> (also in refs-before.txt)
git rebase --onto <START> <REVIEWED> <BRANCH>
git switch <START>                           # leave the shared checkout where the cycle began
```

If the rebase stops on a conflict, resolve it only if the conflict comes straight from your fixes and the resolution is obvious. Otherwise, or if anything looks unclear, abort (`git rebase --abort`) and go off track.

### Verify with git

```bash
git status                                                   # clean, no rebase in progress
git rev-list --count <BASE>..<START>                         # 1: still a single, squashed commit
git range-diff <REVIEWED>..<OLD_BRANCH> <START>..<BRANCH>    # with a branch
git for-each-ref --format='%(refname) %(objectname)' refs/heads > "$SCRATCH/refs-after.txt"
diff "$SCRATCH/refs-before.txt" "$SCRATCH/refs-after.txt"
```

The range-diff must pair every old branch commit with a rebased one (`=`, or `!` where a conflict was resolved, which you should reread). A `<` line means a commit was lost. The ref diff may show only `<START>` changing, plus `<BRANCH>` when one was named. If no round accepted anything, nothing should have changed. Anything else is off track.

## Report

Give the user a short summary:

- **Rounds:** how many ran, and why the cycle stopped (converged, or round limit).
- **Per round:** findings raised by each reviewer, which you fixed, and which you rejected or found already addressed, with a phrase of reasoning each.
- **Commit:** the final hash and message subject.
- **Rebase:** the state of the rebased branch, or that no rebase was requested.

Say plainly if a step was skipped, a check was not run, or the last round's fixes went unreviewed because the round limit was reached. In that case, offer to run another round.

## Off track

Stop and tell the user what you saw when any of these happens:

- A reviewer is `blocked` on an approval or question dialog. Read it, but never answer it yourself. The one exception is the scripted Codex "skill isn't installed" reply above.
- A wait times out, or a reviewer sits `working` far past the expected time.
- A review has no recognizable final report.
- Your fixes fail validation and you can't resolve it.
- The rebase hits a conflict you can't resolve, or leaves a dirty tree.
- Any branch other than `<START>` and the named branch moves.
- At the start, `HEAD` is not the commit the user meant, `HEAD` is detached, or the tree is dirty.

Do not retry a mutating step (resending a prompt, re-running the rebase) until you have inspected the agent's screen and the git state. A timeout or stall does not prove the prompt was not delivered.
