---
name: review-cycle
description: Run the review-fix-commit loop, optionally followed by a rebase, across the herdr agents `reviewer` and `coder`. The reviewer runs a code review, its findings go to the coder to evaluate and fix, then the coder commits, and rebases a branch onto the result if one was named. Use whenever the user asks to run the review cycle, have the reviewer review and the coder fix, or send review findings to the coder, even if they don't name this skill. Before starting, it confirms the `pm`, `coder` (claude) and `reviewer` (codex) tabs and agents, and offers to rename or create any that are missing. Takes an optional branch to rebase; with no branch, the cycle ends at the commit.
---

# Review cycle

You are the project manager (`pm`) agent in a herdr workspace. Two other agents sit in sibling tabs: `reviewer` reviews, `coder` fixes. This skill drives them through one full loop and reports the outcome. It is the user's standing instruction to proceed on your own while each step looks as expected, because the point is to spare them from babysitting the tabs. Stop and ask only when something goes off track (see "Off track" below).

Load the herdr skill first (`herdr --skill`) if it is not already in context. It is the authority on `herdr agent` and `herdr pane` syntax. This skill only records the workflow and the traps found while doing it.

## Inputs

- **Branch** (optional): the branch the coder rebases onto the fix commit, taken from the user's request (for example "rebase remote-exec"). Often a later branch is stacked on the code under review and needs to pick up the fixes. Just as often the cycle ends once the fixes are committed. If no branch was named, skip the rebase rather than asking for one, and say so in your first status line ("no branch given, so no rebase") so the user can correct you early. Do not guess a branch from `git branch`.

## Preconditions

`test "${HERDR_ENV:-}" = 1`, otherwise say you are not inside herdr and stop.

Keep scratch files in the session scratchpad directory, not the repo. The commands below write it as `$SCRATCH`, but shell variables do not survive between Bash calls, so put the scratchpad's absolute path in each command (or begin it with `SCRATCH=<scratchpad path>;`). An empty `$SCRATCH` would turn `"$SCRATCH/reviewer.txt"` into `/reviewer.txt`.

## Confirm the team

The cycle only works if three roles are each in their own tab under the expected name: `pm` (you) runs `claude`, `coder` runs `claude`, and `reviewer` runs `codex`. Every later step sends prompts by agent name, so a wrong or missing name sends work to the wrong place or fails. Settle all three roles before Step 0. Renaming, creating tabs and starting agents change the user's workspace, so offer them and wait for a yes. The standing instruction to proceed on your own starts only after the team is confirmed.

Gather the facts with read-only commands first. Take your pane, tab and workspace IDs from the live `pane current` output, not from `$HERDR_PANE_ID`, `$HERDR_TAB_ID` and `$HERDR_WORKSPACE_ID`. Those are inherited when the pane starts and still name its old location after it has been moved, so a rename through them could hit another tab.

```bash
herdr pane current --current           # your pane: .agent kind, .name if named, .pane_id, .tab_id, .workspace_id
herdr tab get <tab_id>                 # your tab: .label
herdr agent list                       # every live agent: name, agent (kind), pane_id, tab_id, agent_status
herdr tab list --workspace <workspace_id>   # tab labels
```

Use those same three IDs in every command below that names your pane, tab or workspace.

An agent with no name has no `name` field. A tab whose label is just its number (label `"1"` on tab number 1) has herdr's default label, so treat it as unnamed. A role is **confirmed** when one live agent has that name, runs the expected kind, and is the agent in a tab with the same label.

### 1. Who you are

- **Your agent name or tab label is `coder` or `reviewer`:** stop before changing anything. Tell the user what you found and ask whether they ran this from the wrong agent. Do not rename yourself. The user probably meant to type in the `pm` tab, and renaming would leave the team without that role.
- **Your kind is not `claude`:** stop and tell the user. This skill is written for a Claude `pm`, and renaming would only hide the mismatch.
- **Your agent and your tab are both named `pm`, and your kind is `claude`:** confirmed, go on.
- **Anything else** (unnamed, a default number label, some other name, or only one of the two set to `pm`): ask whether you may rename this agent and tab to `pm`. On a yes:

  ```bash
  herdr agent rename <pane_id> pm
  herdr tab rename <tab_id> pm
  ```

  Agent names are unique among live agents, so if another agent is already `pm` the rename fails. Report that and ask rather than renaming the other agent.

### 2. The coder

Look for an agent named `coder` and a tab labelled `coder`.

- **Confirmed** (named `coder`, kind `claude`, in the `coder` tab): go on.
- **Otherwise:** tell the user exactly what is wrong (no `coder` at all, `coder` is running `codex`, the `coder` tab holds an unnamed agent, and so on), then make an offer:
  - If an agent named `coder` runs `claude` but its tab has another label, offer to rename just that tab. If a different tab already has the `coder` label, say so and ask which one to keep.
  - If some other tab is running `claude` and is not you or the reviewer, offer to make it the coder by renaming that agent and its tab. If there are several, list them with their tab label, cwd and status and let the user pick. Say in the offer that Step 0 will `/clear` the chosen agent, so any conversation in it will be lost.

    ```bash
    herdr agent rename <pane_id> coder
    herdr tab rename <tab_id> coder
    ```

  - If no spare tab is running `claude`, offer to create one. Propose your own cwd and mention it, because the coder commits (and rebases, if asked) in whatever checkout it starts in and the user may want a different worktree:

    ```bash
    herdr tab create --workspace <workspace_id> --cwd "$PWD" --label coder --no-focus
    herdr agent start coder --kind claude --pane <.result.root_pane.pane_id>
    ```

  - If an agent named `coder` runs the wrong kind, do not stop or replace it yourself. It may hold work the user cares about. Ask what they want.

### 3. The reviewer

Same checks and offers as the coder, with name and label `reviewer` and kind `codex`. If a spare tab is running `codex`, offer to rename it. If none is, offer to create one. A new codex agent must start with `--approve-for-me` so it can run the review without stopping at a permission prompt for every tool call:

```bash
herdr tab create --workspace <workspace_id> --cwd "$PWD" --label reviewer --no-focus
herdr agent start reviewer --kind codex --pane <.result.root_pane.pane_id> -- --approve-for-me
```

The reviewer reviews `HEAD` of its own checkout and the coder fixes in its own (Step 1 checks they match), so mention the proposed cwd when you offer.

### Asking

Check all three roles before asking, then put every problem and offer to the user in one message. Three separate interruptions for one setup are tedious. The exception is the wrong-agent case in role 1, which stops everything immediately. After acting on the answers, rerun `herdr agent list` and `herdr tab list` and confirm all three roles from the fresh output instead of assuming the commands worked. If `agent start` returns `agent_not_ready`, the new agent is blocked at a startup dialog. Read it with `herdr agent read` and ask the user. Do not answer it yourself.

### Ready check

Once the team is confirmed, `coder` and `reviewer` must both be `idle` (or `done`). If one is busy, inspect it with `herdr agent read` and ask. Sending a prompt into a busy agent mixes two jobs.

Codex takes `$code-review` and rejects the slash form `/code-review` with "Unrecognized command".

### Checkouts

Check both checkouts before Step 0 clears anything. The reviewer always reviews `HEAD` of the checkout it runs in, not the working tree or any named branch, so the code under review must already be committed at `HEAD` there. Its sub-reviewers still read surrounding files from the working tree, and the coder's later "commit this" would sweep up whatever is already in its tree. So both must be at the commit the user means, with nothing staged, modified or untracked:

```bash
git -C <reviewer cwd> rev-parse HEAD
git -C <coder cwd> rev-parse HEAD
git -C <reviewer cwd> status --porcelain    # must print nothing
git -C <coder cwd> status --porcelain       # must print nothing
```

Treat a `HEAD` that is not the commit the user meant, a mismatch between the two, or any output from `status --porcelain` as off track. Do not stash or clean it yourself; it may be the user's work. Call that commit `<REVIEWED>` below.

## Step 0: clear both agents

Start from fresh context so neither agent carries over an earlier review or fix session. Both agents are idle (ready check above), so it is safe to send:

```bash
herdr agent prompt reviewer '/clear'
herdr agent prompt coder '/clear'
```

`/clear` is a local command that resets the conversation and returns to the input box immediately, so do not use `--wait`. Confirm it took effect before moving on: read each screen (`herdr agent read <name> --source visible --lines 40`), check that the earlier conversation is gone, and run `herdr agent list` to see both `idle` again. If an agent's kind rejects `/clear` ("Unrecognized command" or similar), report it to the user rather than guessing at a substitute. Clear the input box with `ctrl+u` if a stale command is left in it.

## Step 1: review

Both checkouts are clean and at `<REVIEWED>` (see "Checkouts"), so the findings' line references match the code the coder edits.

```bash
herdr agent prompt reviewer '$code-review' --wait --timeout 600000
```

Single quotes keep the shell from expanding `$code-review`.

`agent prompt` can report `agent_prompt_stalled` while the text is merely sitting unsubmitted in the input box. Do not resend. Read the screen (`herdr agent read reviewer --source visible --lines 40`); if the input box still holds the text, press Enter with `herdr agent send-keys reviewer enter` and then `herdr agent wait reviewer --timeout 600000`. If the box is empty and the agent is working, the prompt was submitted, so just wait. If a stale, wrong command is in the box, clear it with `ctrl+u` first.

The review fans out to sub-reviewers and takes a few minutes. The first settled state can be a brief idle between sub-agents, so after the wait returns, read the output and confirm the final consolidated report is actually there (a headline like "Reviewed HEAD <sha>" followed by findings) before moving on.

## Step 2: extract the findings

```bash
herdr agent read reviewer --source recent-unwrapped --lines 400 > "$SCRATCH/reviewer.txt"
```

Use `recent-unwrapped` so wrapped lines are joined. Take the final report only, not the tool-call transcript above it.

Keep the findings: the headline, each numbered finding with its file and line references, the recommended-changes list, and any closing assessment of the design. Remove the recap of how the review was run, meaning paragraphs such as "All five reviewers completed…", "Reviewers agreed on…" and "Validation passed…". The coder should judge the findings on their merits, and the process narrative adds noise and borrowed authority. Reviewer wording varies between runs, so apply that rule rather than matching exact strings. Do not paraphrase or reorder the findings; copy them verbatim.

If the report contains no findings, stop here and tell the user the review came back clean. Nothing goes to the coder.

## Step 3: coder fixes

Before the coder touches anything, record its starting branch and a snapshot of every branch ref. Step 4 checks against both, because a rebase leaves the coder's checkout on `<BRANCH>`, and only a before-and-after comparison can show that no unrelated branch moved:

```bash
git -C <coder cwd> branch --show-current    # call this <START>
git -C <coder cwd> for-each-ref --format='%(refname) %(objectname)' refs/heads > "$SCRATCH/refs-before.txt"
```

Branch refs are shared by all worktrees of a repository, so this snapshot also covers branches checked out elsewhere. If `branch --show-current` prints nothing, the coder is on a detached `HEAD` and its commit would land on no branch. Treat that as off track.

Build the prompt in a file and send it. With a branch, replace `<BRANCH>` with it:

```
evaluate the following feedback. fix the valid findings unless they are already addressed by a later commit on branch `<BRANCH>`. ignore the invalid findings.
--
<findings, verbatim>
```

With no branch there is no later work to check against, so drop that clause:

```
evaluate the following feedback. fix the valid findings. ignore the invalid findings.
--
<findings, verbatim>
```

```bash
herdr agent prompt coder "$(cat "$SCRATCH/prompt.txt")" --wait --timeout 900000
```

The coder is told to judge the findings itself, so some may legitimately be rejected or already fixed. That is expected. Read its closing message (`herdr agent read coder --source recent-unwrapped --lines 120`) and note which findings it fixed, rejected, or found already addressed. The same stall-then-press-Enter trap from Step 1 applies.

Proceed when the coder is idle, reports finished work, and `git -C <coder cwd> status` shows its changes. Since the tree was clean at the start, every change there is the coder's. Its closing message must also show that the validation the repo requires for those changes (see `.agents/kb/testing.md`) ran and passed. If it skipped any required check, ask it to run them before Step 4; if any fail, that is off track.

If the coder made no changes (every finding rejected or already addressed), there is nothing to commit. Report that to the user and stop.

## Step 4: commit, and rebase if a branch was named

With a branch (substitute the real name):

```bash
herdr agent prompt coder 'commit this, then rebase `<BRANCH>` onto it' --wait --timeout 900000
```

With no branch:

```bash
herdr agent prompt coder 'commit this' --wait --timeout 900000
```

Afterwards verify with git yourself instead of trusting the coder's summary. Run everything against the coder's checkout, which need not be yours, and name `<START>` explicitly, because after a rebase the coder's checkout is on `<BRANCH>`, not `<START>`:

```bash
git -C <coder cwd> status                                    # clean tree, no rebase in progress
git -C <coder cwd> log --oneline <REVIEWED>..<START>         # the fix commit(s), nothing else
git -C <coder cwd> merge-base --is-ancestor <REVIEWED> <START> && echo on-top
git -C <coder cwd> merge-base --is-ancestor <START> <BRANCH> && echo rebased   # with a branch
git -C <coder cwd> range-diff <START>..<OLD_BRANCH> <START>..<BRANCH>          # with a branch
git -C <coder cwd> for-each-ref --format='%(refname) %(objectname)' refs/heads > "$SCRATCH/refs-after.txt"
diff "$SCRATCH/refs-before.txt" "$SCRATCH/refs-after.txt"
```

`<OLD_BRANCH>` is the SHA `refs/heads/<BRANCH>` had in `refs-before.txt`. The ancestry check alone would also pass if `<BRANCH>` had been reset to the fix commit, so the `range-diff` is what shows its own work survived.

Expected: a clean tree, `<START>` advanced from `<REVIEWED>` by the fix commit, and `on-top` printed. With a branch, also `rebased` printed, and the `range-diff` pairs every old commit with a rebased one (`=`, or `!` where the rebase resolved a conflict, which you should read). A `<` line (a commit only on the old side) means it was lost or dropped, and `fatal: need two commit ranges` means `<BRANCH>` holds no commits past `<START>` at all. Both are off track. The ref diff may show only `<START>` changing, plus `<BRANCH>` when one was named. Any other changed, added or removed ref is off track. Check that the coder's commit message (`git -C <coder cwd> log -1 --format=%B <START>`) follows the repo's conventions in `.agents/kb/development.md`.

## Report

Finish with a short summary for the user: how many findings the reviewer raised, which the coder fixed and which it rejected (and why, in a phrase), the commit hash, and either the state of the rebased branch (noting that the coder's checkout is now on `<BRANCH>`) or that no rebase was requested. Say plainly if a step was skipped or a check was not run.

## Off track

Stop and ask the user, with what you saw, when any of these happens:

- An agent is `blocked` on an approval or question dialog. Read it, and never answer it yourself.
- A wait times out, or an agent sits `working` far past the expected time.
- The review output has no recognizable final report.
- The coder's fixes fail tests, or it asks a question instead of finishing.
- The rebase hits conflicts, stops midway, or leaves a dirty tree.
- Anything touches a branch other than the coder's own and the one the user named (with no branch named, any other branch moving is off track).
- The reviewer's `HEAD` is not the commit the user meant to have reviewed, the coder's `HEAD` differs from it, or either checkout has uncommitted or untracked files.

Do not retry a mutating step (re-sending a prompt, re-running the rebase) until you have inspected the agent's screen and git state. A timeout or stall does not prove the prompt was not delivered.
