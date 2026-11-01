import React from 'react';
import './thanksgiving.css';

// Seasonal design for the public site. A theme restyles pages and adds art; it
// never changes their wording. To change season, add a theme here, point
// ACTIVE_SITE_THEME at it and scope its CSS under its data-site-theme value.
// Set it to null to return to the base brand.

export const ACTIVE_SITE_THEME: string | null = 'thanksgiving';

export const SiteThemeScene: React.FC = () =>
  ACTIVE_SITE_THEME === 'thanksgiving' ? (
    <div className="jv-theme-scene">
      <img src="/themes/thanksgiving/sun.svg" alt="" width={1440} height={460} decoding="async" data-speed="0.2" />
      <img src="/themes/thanksgiving/hills.svg" alt="" width={1440} height={460} decoding="async" data-speed="0.08" />
      <img src="/themes/thanksgiving/homestead.svg" alt="" width={1440} height={460} decoding="async" />
      <img src="/themes/thanksgiving/leaves.svg" alt="" width={1440} height={460} decoding="async" data-speed="-0.14" />
    </div>
  ) : null;
