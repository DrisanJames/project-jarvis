// One localStorage flag: "this browser was signed in last time". It decides
// what shows during the auth check, so a signed-in user sees the loading state
// and not a flash of the marketing site. It grants nothing; /auth/user decides.

const KEY = 'jarvis_signed_in';

export const hasSignInHint = (): boolean => {
  try {
    return localStorage.getItem(KEY) === '1';
  } catch {
    return false;
  }
};

export const setSignInHint = (on: boolean): void => {
  try {
    if (on) localStorage.setItem(KEY, '1');
    else localStorage.removeItem(KEY);
  } catch {
    /* private window: the worst case is a brief flash */
  }
};

export const markSignInStarted = (): void => setSignInHint(true);
