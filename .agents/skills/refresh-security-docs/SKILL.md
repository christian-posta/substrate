---
name: refresh-security-docs
description: Audit and update the root IDENTITY.md, EGRESS.md, RBAC.md, and CREDENTIALS.md against upstream/main source, covering both egress dataplanes, and record the verified upstream commit. Use whenever asked to refresh, update, check, or "make accurate" any of these four docs, to compare them with upstream/main, or to find out what changed upstream in Actor identity, egress, credential injection, or authorization since the docs were last verified.
---

# Refresh the security architecture docs

The four root docs describe Actor identity, egress, credential injection, and authorization. They exist only on this fork. Each run verifies them against **one exact `upstream/main` commit**, then rewrites them to describe that commit. Follow these steps in order every time so that runs are comparable.

## Fixed decisions

These decisions are made in advance. Do not re-decide them in a run.

- **Target**: `upstream/main` at the SHA resolved in step 1. Read source only from a detached worktree at that SHA. The current branch is usually far behind upstream; its source is not evidence. Never rebase, merge, switch, or reset the branch.
- **Baseline**: each doc ends with `<!-- verified-against: agent-substrate/substrate@<sha> <date> -->`. The delta since that SHA is the starting point for the next run. If the markers are missing or disagree, the script falls back to the merge base.
- **Tense**: the docs describe the target commit as current behavior. Do not write "newer upstream", "the pinned …", or "now supports". Write the behavior. Date open issue and PR status ("as of 2026-10-27").
- **Dataplanes**: state which dataplane the installer selects by default. Center the explanation on AgentGateway. Describe Envoy where it differs in a way that changes behavior or a security conclusion.
- **Pinned-dataplane compatibility**: the AgentGateway image is pinned by commit, while Substrate's credential names, gRPC packages, and proto fields keep moving. Every run checks whether that pinned commit still matches the target SHA. At minimum, compare the accepted `*-for-actor` SPIFFE path prefix, the ateapi and credential-provider gRPC package names, and the required request fields. The AgentGateway CI lane is `continue-on-error`, so CI does not catch a mismatch. If it is incompatible, keep AgentGateway as the main explanation of the design. Add one short, dated note per affected doc saying that the pinned image cannot complete the path at this commit and naming the concrete mismatch. Describe Envoy as the installer default that works. Do not describe the AgentGateway path as working, and do not drop it from the docs.
- **Style and scope**: follow the "Substrate architecture notes" section of the workspace `../AGENTS.md`. Use `IDENTITY.md` as the model for density, introduce components before relying on them, use exact component and credential names, and write at the architecture level. Keep consequential gaps, but do not turn the docs into a backlog.
- **Evidence**: an RPC, flag, or proto comment is not a working path. A claim needs the handler, the production caller, and the install default or a test.
- **Output**: edit only the four docs. Do not commit unless the user asks.

## Steps

### 1. Preflight

1. Read `AGENTS.md` and `../AGENTS.md`. Run `git status --short`. If any of the four docs has uncommitted changes, treat those changes as the user's work and build on them.
2. Run `git fetch upstream`.
3. Run the preflight script. It takes about 30 seconds:

   ```
   python3 -I .agents/skills/refresh-security-docs/scripts/preflight.py report --gh --out .agents/scratch/refresh-security-docs
   ```

   The report contains the target SHA, the baseline, watched commits and added, deleted, or renamed files since the baseline, changed upstream docs, backticked identifiers that no longer exist in source (usually renames), broken cross-doc links, the live state of every cited issue and PR, and prose-format problems.
4. Create the worktree at the target SHA:

   ```
   git worktree add --detach .agents/scratch/refresh-security-docs/upstream <sha>
   ```

   If a stale worktree is already at that path, remove it with `git worktree remove --force`.
5. Resolve the AgentGateway commit from the image tag in `manifests/ate-install/components/agentgateway*/kustomization.yaml` at the target SHA. The tag has the form `v0.0.0-alpha.<commit>`. Confirm that `git -C ~/rust/agentgateway cat-file -t <commit>` reports `commit`. If it does not, run `git -C ~/rust/agentgateway fetch` and check again. Never check out or pull in that repository. If the commit is still unavailable, mark the AgentGateway claims as unverifiable rather than reading another commit.

### 2. Verify

Start four verifier subagents in one message, one each for IDENTITY, EGRESS, RBAC, and CREDENTIALS. Use the brief in `references/verifier-brief.md` verbatim, with its placeholders filled in. Each verifier writes `.agents/scratch/refresh-security-docs/ledger-<DOC>.md`. Do not verify the same material yourself while the verifiers run.

If the user asked only whether the docs are accurate, stop after step 3 and report the reconciled findings.

### 3. Reconcile

Read all four ledgers. Check the source yourself when:

- two ledgers disagree, for example on a credential name or the default dataplane;
- a finding is marked **wrong**;
- a finding would change a security conclusion, such as who can mint, what a credential carries, what is checked, or what is denied by default;
- a finding is marked **unverifiable**.

Build one rename map from the preflight report and the ledgers, and apply it to all four docs. Collect open questions for the author. Do not guess the answers.

### 4. Edit

- Apply corrections, renames, and missing facts in place. Restructure a section only when its reading order no longer matches the system: for example, when a new component must be introduced before the section can rely on it.
- Keep each prose paragraph on one source line. Keep tables wherever they compare identities, hops, or dataplanes.
- Refresh status tables from the preflight `gh` states with today's date. Describe merged work as behavior, but only if the merged code is reachable at the target SHA. Drop rows for work that is now plain behavior, unless the link still helps the reader.
- Keep the cross-references among the four docs consistent: the same names, the same default dataplane, and the same gap statements.

### 5. Independent review

Start one fresh reviewer subagent. Give it the four edited docs and the worktree path, but not the ledgers. Ask it to find claims that source at the SHA contradicts or does not support, with `path:line` evidence. Check each finding before acting on it. Fix the confirmed ones.

### 6. Stamp and recheck

```
python3 -I .agents/skills/refresh-security-docs/scripts/preflight.py stamp <sha>
python3 -I .agents/skills/refresh-security-docs/scripts/preflight.py report --out .agents/scratch/refresh-security-docs
```

The recheck must report no missing identifiers, no link problems, and no version-relative phrases. A remaining item is acceptable only if it is intentional, such as a proposed field name in a dated status row. List each such exception in the final report. Then remove the worktree with `git worktree remove .agents/scratch/refresh-security-docs/upstream`.

### 7. Report

Lead with the baseline and target SHAs and the number of commits between them. Then give:

- the consequential behavior changes, per doc, in a few bullets each;
- the renames you applied;
- the open questions for the author;
- anything you could not verify, and the reason;
- the exceptions accepted in step 6.

Do not paste the docs into the report. If the user asks for a commit, follow the commit-message rules in `AGENTS.md`. Do not include issue or PR references in the message.

## Maintaining this skill

If a run finds a component that affects these topics in a path that `WATCH_PATHS` in `scripts/preflight.py` does not cover, add the path. If the author corrects a recurring judgment call, add it to **Fixed decisions** so that the next run makes the same call without being told.
