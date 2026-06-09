import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

// Transforms ```mermaid code blocks into <pre class="mermaid"> for client-side rendering.
// Avoids rehype-mermaid's playwright/mermaid-isomorphic dependency which breaks in CI.
function rehypeMermaidPre() {
  return function(tree) {
    function walk(node) {
      if (!node.children) return;
      for (let i = 0; i < node.children.length; i++) {
        const child = node.children[i];
        if (
          child.type === 'element' &&
          child.tagName === 'pre' &&
          child.children?.[0]?.tagName === 'code' &&
          (child.children[0].properties?.className ?? []).includes('language-mermaid')
        ) {
          const text = child.children[0].children?.[0]?.value ?? '';
          node.children[i] = {
            type: 'element',
            tagName: 'pre',
            properties: { className: ['mermaid'] },
            children: [{ type: 'text', value: text }],
          };
        } else {
          walk(child);
        }
      }
    }
    walk(tree);
  };
}

export default defineConfig({
  markdown: {
    syntaxHighlight: {
      type: 'shiki',
      excludeLangs: ['mermaid'],
    },
    rehypePlugins: [rehypeMermaidPre],
  },
  integrations: [
    starlight({
      title: 'Learn Agent Substrate',
      description: 'A visual atlas of how agent-substrate works.',
      customCss: ['./src/styles/atlas.css'],
      social: {
        github: 'https://github.com/agent-substrate/substrate',
      },
      head: [
        {
          tag: 'script',
          attrs: { type: 'module' },
          content: `
            import mermaid from 'https://cdn.jsdelivr.net/npm/mermaid@11/dist/mermaid.esm.min.mjs';
            // Diagrams are authored for a light canvas. We keep mermaid pinned to
            // its 'default' (light) theme regardless of site theme - the page CSS
            // gives the diagram its own light card background. This trades a tiny
            // bit of "the diagram feels separate from the page" for vastly better
            // legibility (default mermaid dark theme has terrible contrast on a
            // dark site background).
            mermaid.initialize({
              startOnLoad: false,
              theme: 'default',
              securityLevel: 'loose',
              flowchart: { htmlLabels: true, curve: 'basis' },
              themeVariables: { fontFamily: 'ui-sans-serif, system-ui, sans-serif' },
            });
            await mermaid.run({ querySelector: 'pre.mermaid' });
          `,
        },
      ],
      sidebar: [
        { label: 'Start here', items: [
          { label: 'Overview', slug: '' },
          { label: 'System topology', slug: 'topology' },
        ]},
        { label: 'Flows', items: [
          { label: 'Create actor', slug: 'flows/create-actor' },
          { label: 'Resume actor', slug: 'flows/resume-actor' },
          { label: 'Suspend actor', slug: 'flows/suspend-actor' },
          { label: 'Request path', slug: 'flows/request-path' },
          { label: 'Golden snapshot', slug: 'flows/golden-snapshot' },
          { label: 'Actor lifecycle', slug: 'flows/actor-lifecycle' },
          { label: 'Worker lifecycle', slug: 'flows/worker-lifecycle' },
        ]},
        { label: 'Components', items: [
          { label: 'ateapi', slug: 'components/ateapi' },
          { label: 'atecontroller', slug: 'components/atecontroller' },
          { label: 'atelet', slug: 'components/atelet' },
          { label: 'ateom-gvisor', slug: 'components/ateom-gvisor' },
          { label: 'atenet', slug: 'components/atenet' },
          { label: 'Workers', slug: 'components/workers' },
          { label: 'Storage', slug: 'components/storage' },
        ]},
        { label: 'Concepts', items: [
          { label: 'Actor', slug: 'concepts/actor' },
          { label: 'ActorTemplate', slug: 'concepts/actortemplate' },
          { label: 'WorkerPool', slug: 'concepts/workerpool' },
          { label: 'Worker', slug: 'concepts/worker' },
          { label: 'Snapshot', slug: 'concepts/snapshot' },
          { label: 'Session', slug: 'concepts/session' },
        ]},
      ],
    }),
  ],
});
