# Substrate Atlas

A visual atlas of how [agent-substrate](../) works — control plane, data plane,
lifecycle, request path, snapshot/restore. Built as a static site so you can
open it any time without standing up infrastructure.

## Run it locally

```bash
cd atlas
npm install        # first time only
npm run dev        # starts dev server at http://localhost:4321
```

To produce a static bundle you can host anywhere:

```bash
npm run build      # output in ./dist
npm run preview    # serve the build locally
```

## What's in here

```
src/content/docs/
  index.mdx              Hero + entry point
  topology.mdx           Full system topology
  flows/                 Sequence + state diagrams of the main flows
  components/            Per-component deep-dives
  concepts/              Glossary of domain terms (actor, worker, snapshot, ...)
```

## Stack

- **[Astro](https://astro.build)** + **[Starlight](https://starlight.astro.build)** —
  static docs site framework. Sidebar, search, dark mode out of the box.
- **[Mermaid](https://mermaid.js.org)** — diagrams as text, rendered client-side.
  The `pre-mermaid` rehype strategy keeps `mermaid` code blocks intact at build
  time; a small head script then runs `mermaid.run()` on every page.
- **Excalidraw** (Cut 3) — for the hand-drawn hero topology. Sources live under
  `excalidraw-sources/`, exported SVGs under `src/assets/`.

## Delivery cuts

This atlas was built in three cuts so each one is independently reviewable.

- **Cut 1 (current)**: scaffold + topology + resume-actor flow + ateapi
  deep-dive + this README.
- **Cut 2**: all remaining flow pages + state machines.
- **Cut 3**: all remaining component + concept pages, polished Excalidraw hero.

## Adding new pages

1. Drop an `.mdx` file under `src/content/docs/<section>/<slug>.mdx` with the
   frontmatter:

   ```yaml
   ---
   title: My page
   description: One-liner shown in search results.
   ---
   ```

2. The sidebar entry is auto-generated for `flows/`, `components/`, and
   `concepts/`. Anything else needs an entry in `astro.config.mjs`'s
   `sidebar` array.

3. Use ` ```mermaid ` fences for diagrams. Clickable nodes use:

   ```
   click NodeId "/path/to/page/" "Optional tooltip"
   ```

## Conventions

- Every claim about the codebase is paired with a `file:line` reference
  styled as `<code class="fileref">...</code>` (a small visual tag). Treat
  them as load-bearing — if you change the code, fix the atlas.
- Prefer **Mermaid** for anything procedural (sequence, state, flow). Reach
  for **Excalidraw** when you need an organic, whiteboard-style overview that
  Mermaid would render too rigidly.
- Keep pages narrow: one diagram + just enough prose to read it. Cross-link
  liberally; don't duplicate.
