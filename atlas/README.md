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
  security.mdx           Identity, authentication, authorization, egress
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
- **Hand-drawn SVGs** under `src/assets/` for the hero topology and the
  resume and golden-snapshot illustrations. They are edited as SVG source and
  imported with `?raw` so their boxes stay clickable.

## Keeping it current

The atlas describes upstream `main` as of commit `59a58a5fb` (2026-10-25).
To resync, diff upstream since that commit, update the affected pages, and
move this marker forward. Verify claims against the code and the checked-in
install manifests rather than `docs/architecture.md`, which is partly
aspirational.

Mermaid renders in the browser, so `npm run build` does not catch a broken
diagram. Parse every `mermaid` block (for example with `mermaid.parse` under
jsdom) before publishing.

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

- Describe the architecture and current behavior, not lines of code. Pages
  name binaries, resources, and RPCs but carry no `file:line` references.
- The router and egress gateway run AgentGateway or Envoy. Envoy is the
  install default; the atlas centers the AgentGateway path and calls out Envoy
  where it behaves differently.
- Prefer **Mermaid** for anything procedural (sequence, state, flow). Reach
  for a hand-drawn SVG when you need an organic, whiteboard-style overview that
  Mermaid would render too rigidly.
- Keep pages narrow: one diagram + just enough prose to read it. Cross-link
  liberally; don't duplicate.
