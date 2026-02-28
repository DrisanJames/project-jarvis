import React, { createContext, useContext, useState, useEffect, useCallback, ReactNode } from 'react';

// =============================================================================
// TYPES
// =============================================================================

export interface User {
  id: string;
  email: string;
  name: string;
  picture: string;
  domain: string;
}

export interface Organization {
  id: string;
  name: string;
  domain: string;
}

interface AuthContextType {
  user: User | null;
  organization: Organization | null;
  isAuthenticated: boolean;
  isLoading: boolean;
  error: string | null;
  login: () => void;
  logout: () => void;
  refreshAuth: () => Promise<void>;
}

// =============================================================================
// CONTEXT
// =============================================================================

const AuthContext = createContext<AuthContextType | undefined>(undefined);

// =============================================================================
// PROVIDER
// =============================================================================

interface AuthProviderProps {
  children: ReactNode;
}

export const AuthProvider: React.FC<AuthProviderProps> = ({ children }) => {
  const [user, setUser] = useState<User | null>(null);
  const [organization, setOrganization] = useState<Organization | null>(null);
  const [isLoading, setIsLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const setDevUser = useCallback(() => {
    const devUser: User = {
      id: 'dev-user',
      email: 'dev@ignitemediagroup.co',
      name: 'Developer',
      picture: '',
      domain: 'ignitemediagroup.co',
    };
    const devOrg: Organization = {
      id: '00000000-0000-0000-0000-000000000001',
      name: 'Jarvis',
      domain: 'ignitemediagroup.co',
    };
    setUser(devUser);
    setOrganization(devOrg);
  }, []);

  const checkAuth = useCallback(async () => {
    try {
      setIsLoading(true);
      setError(null);

      const response = await fetch('/auth/user', {
        credentials: 'include',
      });

      const contentType = response.headers.get('content-type');
      const isJson = contentType && contentType.includes('application/json');

      if (!isJson) {
        console.log('Auth endpoint not available (non-JSON response)');
        if (window.location.hostname === 'localhost' || window.location.hostname === '127.0.0.1') {
          console.log('Dev mode: using dev user fallback');
          setDevUser();
          return;
        }
        setUser(null);
        setOrganization(null);
        return;
      }

      if (response.status === 404) {
        setUser(null);
        setOrganization(null);
        return;
      }

      if (!response.ok) {
        try {
          await response.json();
        } catch {
        }
        if (window.location.hostname === 'localhost' || window.location.hostname === '127.0.0.1') {
          console.log('Dev mode: using dev user fallback (auth returned ' + response.status + ')');
          setDevUser();
          return;
        }
        setUser(null);
        setOrganization(null);
        return;
      }

      const data = await response.json();

      if (data.authenticated && data.user) {
        const userData: User = {
          id: data.user.id || data.user.email,
          email: data.user.email,
          name: data.user.name,
          picture: data.user.picture || '',
          domain: data.user.domain || data.user.email.split('@')[1],
        };
        setUser(userData);

        const orgData: Organization = {
          id: data.organization?.id || '00000000-0000-0000-0000-000000000001',
          name: data.organization?.name || 'Jarvis',
          domain: userData.domain,
        };
        setOrganization(orgData);
      } else {
        setUser(null);
        setOrganization(null);
      }
    } catch (err) {
      console.error('Auth check failed:', err);
      if (window.location.hostname === 'localhost' || window.location.hostname === '127.0.0.1') {
        console.log('Dev mode: using dev user fallback (network error)');
        setDevUser();
      } else {
        setUser(null);
        setOrganization(null);
        setError('Unable to reach authentication server. Please try again.');
      }
    } finally {
      setIsLoading(false);
    }
  }, [setDevUser]);

  useEffect(() => {
    checkAuth();

    const params = new URLSearchParams(window.location.search);
    const errorParam = params.get('error');
    if (errorParam) {
      let errorMessage = 'Authentication failed';
      switch (errorParam) {
        case 'domain_not_allowed':
          errorMessage = 'Access denied. Only @ignitemediagroup.co accounts are allowed.';
          break;
        case 'invalid_state':
          errorMessage = 'Invalid authentication state. Please try again.';
          break;
        case 'exchange_failed':
          errorMessage = 'Failed to complete authentication. Please try again.';
          break;
        default:
          errorMessage = `Authentication error: ${errorParam}`;
      }
      setError(errorMessage);
      window.history.replaceState({}, '', window.location.pathname);
    }
  }, [checkAuth]);

  const login = useCallback(() => {
    window.location.href = '/auth/login';
  }, []);

  const logout = useCallback(async () => {
    window.location.href = '/auth/logout';
  }, []);

  const value: AuthContextType = {
    user,
    organization,
    isAuthenticated: !!user,
    isLoading,
    error,
    login,
    logout,
    refreshAuth: checkAuth,
  };

  return (
    <AuthContext.Provider value={value}>
      {children}
    </AuthContext.Provider>
  );
};

// =============================================================================
// HOOK
// =============================================================================

export const useAuth = (): AuthContextType => {
  const context = useContext(AuthContext);
  if (context === undefined) {
    throw new Error('useAuth must be used within an AuthProvider');
  }
  return context;
};

// =============================================================================
// API HELPER
// =============================================================================

/**
 * Creates headers with authentication and organization context
 * Use this for all authenticated API calls
 */
export const getAuthHeaders = (organization: Organization | null): HeadersInit => {
  const headers: HeadersInit = {
    'Content-Type': 'application/json',
  };

  if (organization) {
    headers['X-Organization-ID'] = organization.id;
  }

  return headers;
};

/**
 * Wrapper for fetch that includes auth headers
 */
export const authFetch = async (
  url: string,
  options: RequestInit = {},
  organization: Organization | null
): Promise<Response> => {
  const headers = {
    ...getAuthHeaders(organization),
    ...(options.headers || {}),
  };

  return fetch(url, {
    ...options,
    headers,
    credentials: 'include',
  });
};

export default AuthContext;
