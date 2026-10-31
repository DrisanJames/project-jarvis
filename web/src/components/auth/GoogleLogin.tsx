import React, { Suspense, lazy, useEffect } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faSpinner } from '@fortawesome/free-solid-svg-icons';
import { useAuth } from '../../contexts/AuthContext';
import { hasSignInHint, setSignInHint } from '../marketing/signInHint';
import { prerenderedHtml } from '../../prerenderSnapshot';

// Signed-out visitors get the public marketing site, sign-in page included.
// Who may enter the portal is decided in internal/auth/auth.go.
const MarketingSite = lazy(() => import('../marketing/MarketingSite'));

interface GoogleLoginProps {
  children: React.ReactNode;
}

export const GoogleLogin: React.FC<GoogleLoginProps> = ({ children }) => {
  const { isAuthenticated, isLoading, error, login } = useAuth();

  useEffect(() => {
    if (isLoading) return;
    setSignInHint(isAuthenticated);
    // Prerendered pages ship with this class; the portal must not inherit it.
    if (isAuthenticated) document.body.classList.remove('jv-body');
  }, [isLoading, isAuthenticated]);

  if (isLoading && (hasSignInHint() || !prerenderedHtml)) {
    return (
      <div style={styles.container}>
        <div style={styles.loadingBox}>
          <FontAwesomeIcon icon={faSpinner} spin style={{ ...styles.spinner, color: 'var(--accent-blue, var(--j-blue))', fontSize: '48px' }} />
          <p style={styles.loadingText}>Checking authentication...</p>
        </div>
      </div>
    );
  }

  if (isLoading || !isAuthenticated) {
    // The prerendered page stays on screen until the marketing bundle is ready.
    const snapshot = <div dangerouslySetInnerHTML={{ __html: prerenderedHtml }} />;
    if (isLoading) return snapshot;
    return (
      <Suspense fallback={snapshot}>
        <MarketingSite onSignIn={login} authError={error} />
      </Suspense>
    );
  }

  // Authenticated - render children
  return <>{children}</>;
};

const styles: Record<string, React.CSSProperties> = {
  container: {
    minHeight: '100vh',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    backgroundColor: 'var(--bg-primary, #0f0f1a)',
    padding: '1rem',
  },
  loadingBox: {
    textAlign: 'center' as const,
  },
  spinner: {
    animation: 'spin 1s linear infinite',
  },
  loadingText: {
    marginTop: '1rem',
    color: 'var(--text-muted, var(--j-text-muted))',
    fontSize: '0.875rem',
  },
};

export default GoogleLogin;
