#!/usr/bin/env python3
# Copyright 2026 The Agent Substrate Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Mechanical checks for the root security architecture docs.

Run from the repository root:

  preflight.py report [--ref upstream/main] [--gh] [--out DIR]
  preflight.py stamp SHA

`report` prints the audit baseline, the upstream delta since that baseline,
backticked identifiers that no longer appear in non-Markdown source at the
target ref, broken cross-document links, cited GitHub issues and PRs, and
prose-format problems. `stamp` records the verified commit in every doc.
"""

import argparse
import json
import os
import re
import subprocess
import sys
from datetime import date

DOCS = ["IDENTITY.md", "EGRESS.md", "RBAC.md", "CREDENTIALS.md"]
UPSTREAM_REPO = "agent-substrate/substrate"
MARKER_RE = re.compile(r"<!-- verified-against: [\w/.-]+@([0-9a-f]{7,40}) (\d{4}-\d{2}-\d{2}) -->")

# Source areas whose changes can alter identity, egress, credential, or
# authorization behavior. Extend this when upstream adds a component.
WATCH_PATHS = [
    "api",
    "pkg/proto",
    "cmd/ateapi",
    "cmd/atelet",
    "cmd/atenet",
    "cmd/ate-idp-server",
    "cmd/ate-setup",
    "cmd/credential-provider",
    "cmd/dataplane",
    "cmd/kubectl-ate",
    "cmd/podcertcontroller",
    "internal/actoridjwt",
    "internal/ateapiauth",
    "internal/atunnel",
    "internal/clustertrustbundle",
    "internal/credbundle",
    "internal/egresspolicy",
    "internal/localca",
    "internal/localjwtauthority",
    "internal/oidcdiscovery",
    "internal/oidcjwt",
    "internal/principal",
    "internal/substratex509",
    "manifests/ate-install",
]

# Phrases that describe the docs relative to an unstated version. The docs
# describe one recorded upstream commit, so these usually need rewording.
RELATIVE_PHRASES = [
    r"\bnewer upstream\b",
    r"\bupstream now\b",
    r"\bnow (supports?|has|uses)\b",
    r"\bthe pinned\b(?! AgentGateway image)",
    r"\bcurrently\b(?! (holds|assigned|running))",
    r"\bno longer\b",
]

SKIP_TOKENS = {"kubectl", "kubectl ate", "ateapi", "atelet", "atunnel", "default"}


def git(*args, check=True):
    r = subprocess.run(["git", *args], capture_output=True, text=True)
    if check and r.returncode != 0:
        sys.exit(f"git {' '.join(args)} failed: {r.stderr.strip()}")
    return r.stdout


def read_docs():
    out = {}
    for d in DOCS:
        if not os.path.exists(d):
            sys.exit(f"{d} not found; run from the repository root")
        with open(d) as f:
            out[d] = f.read().split("\n")
    return out


def baseline(docs, ref):
    found = {}
    for d, lines in docs.items():
        for line in lines:
            m = MARKER_RE.search(line)
            if m:
                found[d] = (m.group(1), m.group(2))
    shas = {v[0] for v in found.values()}
    if len(found) == len(DOCS) and len(shas) == 1:
        sha, when = next(iter(found.values()))
        return sha, f"verified-against marker ({when})", found
    mb = git("merge-base", "HEAD", ref).strip()
    why = "no consistent verified-against marker; fell back to merge-base of HEAD and the ref"
    return mb, why, found


def fragments(token):
    """Literal pieces of a backticked token worth searching for."""
    token = re.sub(r"<[^>]*>", " ", token)
    pieces = re.split(r"[\s/]+|://|[=,()\[\]{}\"']", token)
    out = []
    for p in pieces:
        p = p.strip(".:;`*")
        if len(p) < 4 or p.lower() in SKIP_TOKENS:
            continue
        m = re.fullmatch(r"[A-Z]\w+\.([A-Z]\w+)", p)  # Service.Method
        if m:
            out.append(m.group(1))
        elif re.fullmatch(r"[a-z]\w*(\.[a-zA-Z]\w*)+", p):  # YAML/JSON field path
            out.extend(seg for seg in p.split(".") if len(seg) >= 4)
        else:
            out.append(p)
    return out


def missing_identifiers(docs, sha):
    cache = {}
    missing = []
    for d, lines in docs.items():
        in_fence = False
        for n, line in enumerate(lines, 1):
            if line.startswith("```"):
                in_fence = not in_fence
                continue
            for token in re.findall(r"`([^`]+)`", line):
                for frag in fragments(token):
                    if frag not in cache:
                        r = subprocess.run(
                            ["git", "grep", "-q", "-F", "-e", frag, sha, "--", ".", ":(exclude)*.md"],
                            capture_output=True,
                        )
                        cache[frag] = r.returncode == 0
                    if not cache[frag]:
                        missing.append((d, n, token, frag))
    return missing


def slug(heading):
    s = heading.strip().lower()
    s = re.sub(r"[^\w\- ]", "", s)
    return s.replace(" ", "-")


def link_problems(docs):
    anchors = {d: {slug(l.lstrip("#")) for l in lines if l.startswith("#")} for d, lines in docs.items()}
    problems = []
    for d, lines in docs.items():
        for n, line in enumerate(lines, 1):
            for target in re.findall(r"\]\((\./[^)\s]+)\)", line):
                path, _, anchor = target[2:].partition("#")
                if path in anchors:
                    if anchor and anchor not in anchors[path]:
                        problems.append((d, n, target, "anchor not found"))
                elif not os.path.exists(path):
                    problems.append((d, n, target, "file not found"))
    return problems


def github_refs(docs, use_gh):
    refs = {}
    pat = re.compile(r"https://github\.com/" + re.escape(UPSTREAM_REPO) + r"/(issues|pull)/(\d+)")
    for d, lines in docs.items():
        for n, line in enumerate(lines, 1):
            for kind, num in pat.findall(line):
                refs.setdefault(int(num), []).append(f"{d}:{n}")
    rows = []
    for num in sorted(refs):
        status = ""
        if use_gh:
            r = subprocess.run(
                ["gh", "api", f"repos/{UPSTREAM_REPO}/issues/{num}",
                 "--jq", "[.state, (.pull_request.merged_at // \"\"), .title] | @tsv"],
                capture_output=True, text=True,
            )
            if r.returncode == 0:
                state, merged, title = (r.stdout.strip().split("\t") + ["", "", ""])[:3]
                status = f"{'merged' if merged else state} — {title}"
            else:
                status = f"gh error: {r.stderr.strip()[:80]}"
        rows.append((num, status, ", ".join(refs[num])))
    return rows


def prose_problems(docs):
    problems = []
    rel = re.compile("|".join(RELATIVE_PHRASES), re.I)
    for d, lines in docs.items():
        fences = 0
        prev_prose = False
        for n, line in enumerate(lines, 1):
            if line.startswith("```"):
                fences += 1
                prev_prose = False
                continue
            if fences % 2:
                continue
            if line != line.rstrip():
                problems.append((d, n, "trailing whitespace"))
            prose = bool(line.strip()) and not re.match(r"\s*([#|>]|[-*+] |\d+\. |<!--)", line)
            if prose and prev_prose:
                problems.append((d, n, "possible hard wrap (consecutive prose lines)"))
            prev_prose = prose
            for m in rel.finditer(line):
                problems.append((d, n, f"version-relative phrase: '{m.group(0)}'"))
        if fences % 2:
            problems.append((d, 0, "unbalanced code fence"))
    return problems


def report(args):
    docs = read_docs()
    sha = git("rev-parse", args.ref).strip()
    base, why, markers = baseline(docs, args.ref)
    out = []
    p = out.append
    p(f"# Security docs preflight\n")
    p(f"- Target: `{args.ref}` = `{sha}` ({git('log', '-1', '--format=%cs %s', sha).strip()})")
    p(f"- Baseline: `{base}` — {why}")
    for d in DOCS:
        p(f"  - {d}: {markers.get(d, ('none', ''))[0]}")
    count = git("rev-list", "--count", f"{base}..{sha}").strip()
    p(f"- Commits since baseline: {count}\n")

    p("## Watched commits since baseline\n")
    log = git("log", "--no-merges", "--format=%h %cs %s", f"{base}..{sha}", "--", *WATCH_PATHS).strip()
    p("```\n" + (log or "(none)") + "\n```\n")

    p("## Added, deleted, or renamed files in watched paths\n")
    ns = git("diff", "--name-status", "-M", "--diff-filter=ADR", f"{base}..{sha}", "--", *WATCH_PATHS).strip()
    ns = "\n".join(l for l in ns.split("\n") if not re.search(r"(_test\.go|zz_generated|\.pb\.go)$", l))
    p("```\n" + (ns or "(none)") + "\n```\n")

    p("## Upstream docs changed since baseline (leads, not evidence)\n")
    dl = git("diff", "--stat=120", f"{base}..{sha}", "--", "docs/*.md", "docs/threats.json", "AGENTS.md").strip()
    p("```\n" + (dl or "(none)") + "\n```\n")

    p(f"## Backticked identifiers absent from non-Markdown source at `{sha[:9]}`\n")
    miss = missing_identifiers(docs, sha)
    p("\n".join(f"- {d}:{n} `{t}` → `{f}`" for d, n, t, f in miss) or "(none)")
    p("")

    p("## Cross-document link problems\n")
    lp = link_problems(docs)
    p("\n".join(f"- {d}:{n} `{t}` — {why}" for d, n, t, why in lp) or "(none)")
    p("")

    p("## Cited GitHub issues and PRs\n")
    for num, status, where in github_refs(docs, args.gh):
        p(f"- #{num} {status or '(run with --gh for status)'} — {where}")
    p("")

    p("## Prose checks\n")
    pp = prose_problems(docs)
    p("\n".join(f"- {d}:{n} {msg}" for d, n, msg in pp) or "(none)")

    text = "\n".join(out) + "\n"
    if args.out:
        os.makedirs(args.out, exist_ok=True)
        with open(os.path.join(args.out, "preflight.md"), "w") as f:
            f.write(text)
        with open(os.path.join(args.out, "target.json"), "w") as f:
            json.dump({"ref": args.ref, "sha": sha, "baseline": base}, f, indent=2)
    sys.stdout.write(text)


def stamp(args):
    sha = git("rev-parse", args.sha).strip()
    marker = f"<!-- verified-against: {UPSTREAM_REPO}@{sha} {date.today().isoformat()} -->"
    for d in DOCS:
        with open(d) as f:
            text = f.read()
        text = MARKER_RE.sub("", text).rstrip("\n") + "\n\n" + marker + "\n"
        with open(d, "w") as f:
            f.write(text)
    print(f"stamped {', '.join(DOCS)} with {sha}")


def main():
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="cmd", required=True)
    r = sub.add_parser("report")
    r.add_argument("--ref", default="upstream/main")
    r.add_argument("--gh", action="store_true", help="query issue/PR status with gh")
    r.add_argument("--out", help="also write preflight.md and target.json here")
    s = sub.add_parser("stamp")
    s.add_argument("sha")
    args = ap.parse_args()
    report(args) if args.cmd == "report" else stamp(args)


if __name__ == "__main__":
    main()
