// The build writes each marketing page's HTML into #root. React replaces that
// on mount, so keep a copy to hold the page on screen until the auth check and
// the marketing bundle are ready. Empty on the portal's own HTML.
export let prerenderedHtml = '';

export const capturePrerender = (root: HTMLElement): void => {
  prerenderedHtml = root.innerHTML;
};
