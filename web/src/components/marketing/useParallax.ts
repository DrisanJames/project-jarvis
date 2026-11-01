import { useEffect } from 'react';

// Moves [data-speed] layers inside #jv-main as the page scrolls, on every page.
// Transforms only, so nothing reflows, and off entirely for visitors who ask
// for reduced motion. Re-runs when `page` changes because the layers do.
export const useParallax = (page: string) => {
  useEffect(() => {
    const root = document.getElementById('jv-main');
    const motion = window.matchMedia?.('(prefers-reduced-motion: reduce)');
    if (!root || !motion || motion.matches) return;
    const layers = Array.from(root.querySelectorAll<HTMLElement>('[data-speed]'));
    let frame = 0;
    const paint = () => {
      frame = 0;
      const mid = window.innerHeight / 2;
      layers.forEach((el) => {
        const box = el.parentElement!.getBoundingClientRect();
        if (box.bottom < -200 || box.top > window.innerHeight + 200) return;
        const offset = (box.top + box.height / 2 - mid) * Number(el.dataset.speed);
        el.style.transform = `translate3d(0, ${offset.toFixed(1)}px, 0)`;
      });
    };
    const onScroll = () => {
      if (!frame) frame = requestAnimationFrame(paint);
    };
    paint();
    window.addEventListener('scroll', onScroll, { passive: true });
    window.addEventListener('resize', onScroll);
    return () => {
      cancelAnimationFrame(frame);
      window.removeEventListener('scroll', onScroll);
      window.removeEventListener('resize', onScroll);
      layers.forEach((el) => {
        el.style.transform = '';
      });
    };
  }, [page]);
};
