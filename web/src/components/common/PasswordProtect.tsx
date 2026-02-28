import React, { useState, useEffect } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faLock, faEye, faEyeSlash, faExclamationCircle } from '@fortawesome/free-solid-svg-icons';

interface PasswordProtectProps {
  children: React.ReactNode;
  storageKey?: string;
  password?: string;
  title?: string;
  description?: string;
}

const CORRECT_PASSWORD = 'ZGkwFFPE315b3QZ41lFhsV7NGxnN';

export const PasswordProtect: React.FC<PasswordProtectProps> = ({
  children,
  storageKey = 'financials_auth',
  title = 'Password Required',
  description = 'This section is password protected. Please enter the password to continue.',
}) => {
  const [isAuthenticated, setIsAuthenticated] = useState(false);
  const [passwordInput, setPasswordInput] = useState('');
  const [showPassword, setShowPassword] = useState(false);
  const [error, setError] = useState('');
  const [isLoading, setIsLoading] = useState(true);

  // Check if already authenticated on mount
  useEffect(() => {
    const authStatus = sessionStorage.getItem(storageKey);
    if (authStatus === 'authenticated') {
      setIsAuthenticated(true);
    }
    setIsLoading(false);
  }, [storageKey]);

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    setError('');

    if (passwordInput === CORRECT_PASSWORD) {
      sessionStorage.setItem(storageKey, 'authenticated');
      setIsAuthenticated(true);
    } else {
      setError('Incorrect password. Please try again.');
      setPasswordInput('');
    }
  };

  const handleLogout = () => {
    sessionStorage.removeItem(storageKey);
    setIsAuthenticated(false);
    setPasswordInput('');
  };

  if (isLoading) {
    return (
      <div style={styles.loadingContainer}>
        <div style={styles.spinner} />
      </div>
    );
  }

  if (isAuthenticated) {
    return (
      <div style={{ position: 'relative' }}>
        <button
          onClick={handleLogout}
          style={styles.logoutButton}
          title="Lock Financials"
        >
          <FontAwesomeIcon icon={faLock} />
        </button>
        {children}
      </div>
    );
  }

  return (
    <div style={styles.container}>
      <div style={styles.modal}>
        <div style={styles.iconWrapper}>
          <FontAwesomeIcon icon={faLock} style={{ color: 'var(--accent-blue, #3b82f6)', fontSize: '48px' }} />
        </div>
        
        <h2 style={styles.title}>{title}</h2>
        <p style={styles.description}>{description}</p>

        <form onSubmit={handleSubmit} style={styles.form}>
          <div style={styles.inputWrapper}>
            <input
              type={showPassword ? 'text' : 'password'}
              value={passwordInput}
              onChange={(e) => setPasswordInput(e.target.value)}
              placeholder="Enter password"
              style={styles.input}
              autoFocus
            />
            <button
              type="button"
              onClick={() => setShowPassword(!showPassword)}
              style={styles.toggleButton}
            >
              {showPassword ? <FontAwesomeIcon icon={faEyeSlash} /> : <FontAwesomeIcon icon={faEye} />}
            </button>
          </div>

          {error && (
            <div style={styles.error}>
              <FontAwesomeIcon icon={faExclamationCircle} />
              <span>{error}</span>
            </div>
          )}

          <button type="submit" style={styles.submitButton}>
            Unlock
          </button>
        </form>

        <p style={styles.hint}>
          Contact your administrator if you've forgotten the password.
        </p>
      </div>
    </div>
  );
};

const styles: Record<string, React.CSSProperties> = {
  container: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    minHeight: 'calc(100vh - 200px)',
    padding: '2rem',
  },
  loadingContainer: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    minHeight: 'calc(100vh - 200px)',
  },
  spinner: {
    width: '40px',
    height: '40px',
    border: '3px solid var(--border-color, #333)',
    borderTopColor: 'var(--accent-blue, #3b82f6)',
    borderRadius: '50%',
    animation: 'spin 1s linear infinite',
  },
  modal: {
    backgroundColor: 'var(--bg-secondary, #1e1e2e)',
    border: '1px solid var(--border-color, #333)',
    borderRadius: '16px',
    padding: '2.5rem',
    maxWidth: '400px',
    width: '100%',
    textAlign: 'center' as const,
    boxShadow: '0 25px 50px -12px rgba(0, 0, 0, 0.25)',
  },
  iconWrapper: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    width: '80px',
    height: '80px',
    margin: '0 auto 1.5rem',
    backgroundColor: 'rgba(59, 130, 246, 0.1)',
    borderRadius: '50%',
  },
  title: {
    margin: '0 0 0.5rem 0',
    fontSize: '1.5rem',
    fontWeight: 600,
    color: 'var(--text-primary, #fff)',
  },
  description: {
    margin: '0 0 2rem 0',
    fontSize: '0.875rem',
    color: 'var(--text-muted, #888)',
    lineHeight: 1.5,
  },
  form: {
    display: 'flex',
    flexDirection: 'column' as const,
    gap: '1rem',
  },
  inputWrapper: {
    position: 'relative' as const,
    display: 'flex',
    alignItems: 'center',
  },
  input: {
    width: '100%',
    padding: '0.875rem 3rem 0.875rem 1rem',
    fontSize: '1rem',
    backgroundColor: 'var(--bg-tertiary, #2a2a3e)',
    border: '1px solid var(--border-color, #333)',
    borderRadius: '8px',
    color: 'var(--text-primary, #fff)',
    outline: 'none',
    transition: 'border-color 0.2s',
  },
  toggleButton: {
    position: 'absolute' as const,
    right: '0.75rem',
    background: 'none',
    border: 'none',
    color: 'var(--text-muted, #888)',
    cursor: 'pointer',
    padding: '0.25rem',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
  },
  error: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    gap: '0.5rem',
    padding: '0.75rem',
    backgroundColor: 'rgba(239, 68, 68, 0.1)',
    border: '1px solid var(--accent-red, #ef4444)',
    borderRadius: '8px',
    color: 'var(--accent-red, #ef4444)',
    fontSize: '0.875rem',
  },
  submitButton: {
    padding: '0.875rem',
    fontSize: '1rem',
    fontWeight: 600,
    backgroundColor: 'var(--accent-blue, #3b82f6)',
    color: 'white',
    border: 'none',
    borderRadius: '8px',
    cursor: 'pointer',
    transition: 'background-color 0.2s',
  },
  hint: {
    marginTop: '1.5rem',
    fontSize: '0.75rem',
    color: 'var(--text-muted, #888)',
  },
  logoutButton: {
    position: 'absolute' as const,
    top: '1rem',
    right: '1rem',
    display: 'flex',
    alignItems: 'center',
    gap: '0.375rem',
    padding: '0.5rem 0.75rem',
    fontSize: '0.75rem',
    backgroundColor: 'var(--bg-tertiary, #2a2a3e)',
    color: 'var(--text-muted, #888)',
    border: '1px solid var(--border-color, #333)',
    borderRadius: '6px',
    cursor: 'pointer',
    zIndex: 10,
  },
};

export default PasswordProtect;
