import React from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faBolt, faExclamationCircle, faSpinner } from '@fortawesome/free-solid-svg-icons';
import { useAuth } from '../../contexts/AuthContext';

interface GoogleLoginProps {
  children: React.ReactNode;
}

export const GoogleLogin: React.FC<GoogleLoginProps> = ({ children }) => {
  const { isAuthenticated, isLoading, error, login } = useAuth();

  // Show loading state
  if (isLoading) {
    return (
      <div style={styles.container}>
        <div style={styles.loadingBox}>
          <FontAwesomeIcon icon={faSpinner} spin style={{ ...styles.spinner, color: 'var(--accent-blue, #3b82f6)', fontSize: '48px' }} />
          <p style={styles.loadingText}>Checking authentication...</p>
        </div>
      </div>
    );
  }

  // Show login screen if not authenticated
  if (!isAuthenticated) {
    return (
      <div style={styles.container}>
        <div style={styles.loginBox}>
          {/* Logo */}
          <div style={styles.logoContainer}>
            <FontAwesomeIcon icon={faBolt} style={{ color: 'var(--accent-yellow, #facc15)', fontSize: '48px' }} />
          </div>
          
          <h1 style={styles.title}>Jarvis Mailing Platform</h1>
          <p style={styles.subtitle}>
            Sign in with your Google Workspace account to access the monitoring portal.
          </p>

          {/* Error message */}
          {error && (
            <div style={styles.errorBox}>
              <FontAwesomeIcon icon={faExclamationCircle} />
              <span>{error}</span>
            </div>
          )}

          {/* Google Sign In Button */}
          <button onClick={login} style={styles.googleButton}>
            <svg width="18" height="18" viewBox="0 0 18 18" xmlns="http://www.w3.org/2000/svg">
              <g fill="none" fillRule="evenodd">
                <path d="M17.64 9.2c0-.637-.057-1.251-.164-1.84H9v3.481h4.844c-.209 1.125-.843 2.078-1.796 2.717v2.258h2.908c1.702-1.567 2.684-3.874 2.684-6.615z" fill="#4285F4"/>
                <path d="M9.003 18c2.43 0 4.467-.806 5.956-2.18l-2.908-2.259c-.806.54-1.837.86-3.048.86-2.344 0-4.328-1.584-5.036-3.711H.96v2.332A8.997 8.997 0 0 0 9.003 18z" fill="#34A853"/>
                <path d="M3.964 10.71c-.18-.54-.282-1.117-.282-1.71s.102-1.17.282-1.71V4.958H.957A8.996 8.996 0 0 0 0 9c0 1.452.348 2.827.957 4.042l3.007-2.332z" fill="#FBBC05"/>
                <path d="M9.003 3.58c1.321 0 2.508.454 3.44 1.345l2.582-2.58C13.464.891 11.428 0 9.002 0A8.997 8.997 0 0 0 .96 4.958l3.007 2.332c.708-2.127 2.692-3.71 5.036-3.71z" fill="#EA4335"/>
              </g>
            </svg>
            <span>Sign in with Google</span>
          </button>

          <p style={styles.domainNote}>
            Only <strong>@jamesventurescorp.com</strong> accounts are allowed.
          </p>
        </div>
      </div>
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
    color: 'var(--text-muted, #888)',
    fontSize: '0.875rem',
  },
  loginBox: {
    backgroundColor: 'var(--bg-secondary, #1e1e2e)',
    border: '1px solid var(--border-color, #333)',
    borderRadius: '16px',
    padding: '3rem',
    maxWidth: '420px',
    width: '100%',
    textAlign: 'center' as const,
    boxShadow: '0 25px 50px -12px rgba(0, 0, 0, 0.5)',
  },
  logoContainer: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    width: '80px',
    height: '80px',
    margin: '0 auto 1.5rem',
    backgroundColor: 'rgba(250, 204, 21, 0.1)',
    borderRadius: '50%',
  },
  title: {
    margin: '0 0 0.5rem 0',
    fontSize: '1.75rem',
    fontWeight: 700,
    color: 'var(--text-primary, #fff)',
  },
  subtitle: {
    margin: '0 0 2rem 0',
    fontSize: '0.9375rem',
    color: 'var(--text-muted, #888)',
    lineHeight: 1.6,
  },
  errorBox: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    gap: '0.5rem',
    padding: '0.875rem',
    marginBottom: '1.5rem',
    backgroundColor: 'rgba(239, 68, 68, 0.1)',
    border: '1px solid var(--accent-red, #ef4444)',
    borderRadius: '8px',
    color: 'var(--accent-red, #ef4444)',
    fontSize: '0.875rem',
  },
  googleButton: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    gap: '0.75rem',
    width: '100%',
    padding: '0.875rem 1.5rem',
    fontSize: '1rem',
    fontWeight: 500,
    backgroundColor: '#fff',
    color: '#333',
    border: 'none',
    borderRadius: '8px',
    cursor: 'pointer',
    transition: 'all 0.2s ease',
    boxShadow: '0 2px 4px rgba(0,0,0,0.2)',
  },
  domainNote: {
    marginTop: '1.5rem',
    fontSize: '0.75rem',
    color: 'var(--text-muted, #888)',
  },
};

export default GoogleLogin;
