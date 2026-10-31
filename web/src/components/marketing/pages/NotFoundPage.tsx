import React from 'react';
import { JarvisMark } from '../JarvisLogo';
import { PageLink } from '../parts';

export const NotFoundPage: React.FC = () => (
  <section className="jv-section jv-signin">
    <div className="jv-signin-card">
      <JarvisMark size={72} title="" mood="wink" />
      <h1>We couldn’t find that page.</h1>
      <p>It may have moved. These will get you back on track.</p>
      <div className="jv-cta-row jv-cta-row--center">
        <PageLink to="home" className="jv-btn jv-btn--primary">
          Go home
        </PageLink>
        <PageLink to="platform" className="jv-btn jv-btn--ghost">
          Explore the platform
        </PageLink>
      </div>
    </div>
  </section>
);
