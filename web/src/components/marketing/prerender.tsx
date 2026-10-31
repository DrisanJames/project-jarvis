import { renderToStaticMarkup } from 'react-dom/server';
import { PAGES } from './brand';
import { MarketingSite } from './MarketingSite';

// Entry for web/scripts/prerender.mjs: the same components, rendered to HTML at
// build time, so each marketing URL has its text before JavaScript runs.
export { PAGES };

export const renderPage = (path: string): string =>
  renderToStaticMarkup(<MarketingSite onSignIn={() => undefined} initialPath={path} />);
