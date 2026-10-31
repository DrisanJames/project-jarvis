import React from 'react';
import { JarvisMark } from '../JarvisLogo';
import { useMarketingNav } from '../parts';
import { markSignInStarted } from '../signInHint';

export const SignInPage: React.FC<{ error?: string | null }> = ({ error }) => {
  const { signIn } = useMarketingNav();
  return (
    <section className="jv-section jv-signin">
      <div className="jv-signin-card">
        <JarvisMark size={64} title="" animated look />
        <h1>Sign in to Jarvis</h1>
        <p>Use your Google account. If you’re new here, we’ll ask a few questions and reach out.</p>
        {error && (
          <p className="jv-form-error" role="alert">
            {error}
          </p>
        )}
        <button
          type="button"
          className="jv-google"
          onClick={() => {
            markSignInStarted();
            signIn();
          }}
        >
          <svg width="18" height="18" viewBox="0 0 18 18" aria-hidden="true">
            <path d="M17.64 9.2c0-.64-.06-1.25-.16-1.84H9v3.48h4.84a4.14 4.14 0 0 1-1.8 2.72v2.26h2.92c1.7-1.57 2.68-3.87 2.68-6.62z" fill="#4285F4" />
            <path d="M9 18c2.43 0 4.47-.8 5.96-2.18l-2.91-2.26c-.81.54-1.84.86-3.05.86-2.34 0-4.33-1.58-5.04-3.71H.96v2.33A9 9 0 0 0 9 18z" fill="#34A853" />
            <path d="M3.96 10.71a5.41 5.41 0 0 1 0-3.42V4.96H.96a9 9 0 0 0 0 8.08l3-2.33z" fill="#FBBC05" />
            <path d="M9 3.58c1.32 0 2.51.45 3.44 1.35l2.58-2.59A9 9 0 0 0 .96 4.96l3 2.33C4.67 5.16 6.66 3.58 9 3.58z" fill="#EA4335" />
          </svg>
          Continue with Google
        </button>
      </div>
    </section>
  );
};
