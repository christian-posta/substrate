# Verifier brief

Send one verifier per document. Fill in the bracketed values; keep everything else verbatim so each run asks the same questions.

---

You are verifying `[DOC]` in `[REPO_ROOT]` against Agent Substrate source at upstream commit `[SHA]`. Do not edit any file except your ledger.

Inputs:

- Document under review: `[REPO_ROOT]/[DOC]`. Read the other three (`IDENTITY.md`, `EGRESS.md`, `RBAC.md`, `CREDENTIALS.md`) only to understand cross-references.
- Source at the target commit: the detached worktree `[WORKTREE]`. Read source only there. The branch checkout at `[REPO_ROOT]` is older and is not evidence.
- Preflight report: `[SCRATCH]/preflight.md` — watched commits since the last verified baseline, renamed or removed identifiers, and cited issue/PR status.
- AgentGateway source: `~/rust/agentgateway` at commit `[AGW_SHA]`, the commit in the image tag pinned by `manifests/ate-install/components/agentgateway*/`. Read it with `git -C ~/rust/agentgateway show [AGW_SHA]:<path>` or `git grep [AGW_SHA]`; never check out, switch, or pull. Substrate behavior lives under `crates/agentgateway/src/http/substrate/`. If that commit is absent locally, say so and do not substitute the checkout's HEAD.

For every factual claim in `[DOC]` (sentences, table cells, and parenthetical asides), decide one status:

- **confirmed**: source at `[SHA]` shows it is true as written.
- **changed**: still partly true, but a name, default, condition, or scope differs. Give the corrected fact.
- **wrong**: source contradicts it.
- **unverifiable**: you cannot find evidence either way. Say where you looked.

Then list **missing** facts: behavior added since the baseline that changes how a reader understands identity, egress, credentials, or authorization for this document's topic. Use the watched-commit list as your starting point; read the diff for any commit whose subject touches your topic.

Evidence rules:

- Cite `path:line` in the worktree for every status other than confirmed-by-obvious-definition. Cite a test or manifest when the claim is about a default or a deployed path.
- An RPC, proto comment, flag, or type is not a working path. Find the production caller and say whether the default install wires it up (`manifests/ate-install/` and `cmd/ate-setup/`).
- Separate authentication from authorization, and a UID checked at issuance from a UID carried in a credential and checked by its consumer.
- Check both egress dataplanes for any egress or credential claim: Envoy (`cmd/dataplane/envoy`, `cmd/atenet/internal/router/egress`, `cmd/atenet/internal/router/extproc`) and AgentGateway (its source, plus `manifests/ate-install/components/agentgateway*`). Do not infer AgentGateway behavior from `cmd/atenet/internal/router/ingress`.
- State which dataplane the installer selects by default, from `cmd/ate-setup`.
- Upstream `docs/*.md` are leads only. If an upstream doc disagrees with source, source wins; note the disagreement.
- For cited issues and PRs, record state from the preflight report. Do not describe open work as implemented.

Write `[SCRATCH]/ledger-[DOC_STEM].md`:

```
# [DOC] ledger @ [SHA]

| # | Doc line | Claim (short) | Status | Correct fact (if not confirmed) | Evidence |
|---|---|---|---|---|---|

## Missing
| # | Fact | Why a reader needs it | Evidence |

## Renames observed
| Old name | New name | Evidence |

## Open questions
```

Keep each cell to one or two sentences. Reply with the ledger path and a five-line summary: counts per status, the three most consequential corrections, and any open question that needs the author.
