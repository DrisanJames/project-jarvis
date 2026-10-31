// Writes dist/_prerender/<slug>.html (served by spaHandler in
// internal/api/routes.go) and dist/sitemap.xml, so every marketing URL has its
// own metadata and readable text before any JavaScript runs.
import { mkdirSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const ORIGIN = 'https://projectjarvis.io';
const web = join(dirname(fileURLToPath(import.meta.url)), '..');
const dist = join(web, 'dist');
const ssr = join(web, 'dist-ssr');

const entry = readdirSync(ssr).find((f) => /^prerender\.m?js$/.test(f));
const { PAGES, renderPage } = await import(pathToFileURL(join(ssr, entry)).href);

const shell = readFileSync(join(dist, 'index.html'), 'utf8');
const css = readdirSync(join(dist, 'assets')).find((f) => /^MarketingSite-.*\.css$/.test(f));
if (!css) throw new Error('prerender: marketing stylesheet not found in dist/assets');

const attr = (v) => v.replace(/&/g, '&amp;').replace(/"/g, '&quot;');
const swap = (html, re, value) => {
  if (!re.test(html)) throw new Error(`prerender: ${re} not in index.html`);
  return html.replace(re, () => value);
};

mkdirSync(join(dist, '_prerender'), { recursive: true });
for (const page of PAGES) {
  const url = `${ORIGIN}${page.path}`;
  let html = shell;
  html = swap(html, /<title>[^<]*<\/title>/, `<title>${attr(page.title)}</title>`);
  html = swap(html, /<meta name="description"[^>]*>/, `<meta name="description" content="${attr(page.description)}" />`);
  html = swap(html, /<link rel="canonical"[^>]*>/, page.noindex ? '<meta name="robots" content="noindex" />' : `<link rel="canonical" href="${url}" />`);
  html = swap(html, /<meta property="og:title"[^>]*>/, `<meta property="og:title" content="${attr(page.title)}" />`);
  html = swap(html, /<meta property="og:description"[^>]*>/, `<meta property="og:description" content="${attr(page.description)}" />`);
  html = swap(html, /<meta property="og:url"[^>]*>/, `<meta property="og:url" content="${url}" />`);
  html = swap(html, /<\/head>/, `  <link rel="stylesheet" href="/assets/${css}" />\n  </head>`);
  html = swap(html, /<body>/, '<body class="jv-body">');
  html = swap(html, /<div id="root"><\/div>/, `<div id="root">${renderPage(page.path)}</div>`);
  const slug = page.id === 'home' ? 'home' : page.path.slice(1);
  writeFileSync(join(dist, '_prerender', `${slug}.html`), html);
}

const urls = PAGES.filter((p) => !p.noindex).map((p) => `  <url><loc>${ORIGIN}${p.path === '/' ? '/' : p.path}</loc></url>`);
writeFileSync(
  join(dist, 'sitemap.xml'),
  `<?xml version="1.0" encoding="UTF-8"?>\n<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">\n${urls.join('\n')}\n</urlset>\n`,
);

rmSync(ssr, { recursive: true, force: true });
console.log(`prerender: ${PAGES.length} pages, ${urls.length} in sitemap`);
