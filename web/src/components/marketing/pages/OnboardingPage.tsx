import React, { useEffect, useRef, useState } from 'react';
import { JarvisMark } from '../JarvisLogo';
import { Honeypot, SUBMIT_ERROR, USE_CASES, VOLUME_OPTIONS, useFormSubmit } from '../LeadForm';
import { Eyebrow, PageLink } from '../parts';

// The new-customer form. The token comes from the Google sign-in, in the URL
// fragment. It only fills in this form: it is not a session, and finishing the
// form gives no access to the product.

const STORE_KEY = 'jarvis_onboarding';

interface Claims {
  email: string;
  name: string;
}

const readClaims = (token: string): Claims | null => {
  try {
    const part = token.split('.')[0].replace(/-/g, '+').replace(/_/g, '/');
    const json = decodeURIComponent(
      Array.from(atob(part))
        .map((c) => `%${c.charCodeAt(0).toString(16).padStart(2, '0')}`)
        .join(''),
    );
    const p = JSON.parse(json);
    if (!p.e || p.x * 1000 <= Date.now()) return null;
    return { email: p.e, name: p.n ?? '' };
  } catch {
    return null;
  }
};

const takeToken = (): string => {
  const fromHash = new URLSearchParams(window.location.hash.slice(1)).get('t');
  try {
    if (fromHash) {
      sessionStorage.setItem(STORE_KEY, fromHash);
      window.history.replaceState({}, '', window.location.pathname);
    }
    return fromHash ?? sessionStorage.getItem(STORE_KEY) ?? '';
  } catch {
    return fromHash ?? '';
  }
};

const STEPS = ['About you', 'Your business', 'Your email program'];

interface Answers {
  name: string;
  role: string;
  company: string;
  company_site: string;
  volume: string;
  use_case: string;
  current_platform: string;
  notes: string;
  marketing_opt_in: boolean;
}

const Expired: React.FC = () => (
  <div className="jv-form-card">
    <h1>Let’s start again</h1>
    <p className="jv-lede">This form needs a recent Google sign-in.</p>
    <div className="jv-cta-row">
      <PageLink to="signIn" className="jv-btn jv-btn--primary">
        Sign in
      </PageLink>
    </div>
  </div>
);

export const OnboardingPage: React.FC = () => {
  const [token, setToken] = useState('');
  const [claims, setClaims] = useState<Claims | null>(null);
  const [ready, setReady] = useState(false);
  const [step, setStep] = useState(0);
  const [status, setStatus] = useState<'idle' | 'sending' | 'done' | 'error' | 'expired'>('idle');
  const [a, setA] = useState<Answers>({
    name: '',
    role: '',
    company: '',
    company_site: '',
    volume: '',
    use_case: '',
    current_platform: '',
    notes: '',
    marketing_opt_in: false,
  });
  const honeypot = useRef<HTMLInputElement>(null);
  const heading = useRef<HTMLHeadingElement>(null);
  const moved = useRef(false);
  const submit = useFormSubmit();

  useEffect(() => {
    const t = takeToken();
    const c = t ? readClaims(t) : null;
    setToken(t);
    setClaims(c);
    if (c) setA((prev) => ({ ...prev, name: c.name }));
    setReady(true);
  }, []);

  useEffect(() => {
    if (moved.current) heading.current?.focus();
  }, [step]);

  const set = <K extends keyof Answers>(k: K, v: Answers[K]) => setA((prev) => ({ ...prev, [k]: v }));

  const next = (e: React.FormEvent) => {
    e.preventDefault();
    moved.current = true;
    setStep((s) => s + 1);
  };

  const send = async (e: React.FormEvent) => {
    e.preventDefault();
    if (status === 'sending') return;
    setStatus('sending');
    const result = await submit('/api/public/onboarding', {
      ...a,
      onboarding_token: token,
      source_page: '/onboarding',
      website: honeypot.current?.value ?? '',
    });
    if (result === 'ok') {
      try {
        sessionStorage.removeItem(STORE_KEY);
      } catch {
        /* nothing to clear */
      }
      setStatus('done');
    } else {
      setStatus(result === 'unauthorized' ? 'expired' : 'error');
    }
  };

  if (!ready) {
    return (
      <section className="jv-section jv-signin">
        <noscript>
          <h1>Please turn on JavaScript to continue.</h1>
        </noscript>
      </section>
    );
  }

  return (
    <section className="jv-section jv-signin">
      {!claims || status === 'expired' ? (
        <Expired />
      ) : status === 'done' ? (
        <div className="jv-form-card jv-form-card--center" role="status">
          <JarvisMark size={64} title="" mood="wink" />
          <h1>Thanks. We’ll reach out.</h1>
          <p className="jv-lede">We’ve received your details and will email {claims.email}. You’ll have access once we’ve talked.</p>
          <div className="jv-cta-row">
            <PageLink to="home" className="jv-btn jv-btn--ghost">
              Back to home
            </PageLink>
          </div>
        </div>
      ) : (
        <div className="jv-form-card">
          <Eyebrow>
            Step {step + 1} of {STEPS.length}
          </Eyebrow>
          <h1 ref={heading} tabIndex={-1}>
            {STEPS[step]}
          </h1>
          <div className="jv-progress" aria-hidden="true">
            {STEPS.map((s, i) => (
              <span key={s} className={i <= step ? 'is-on' : ''} />
            ))}
          </div>
          <form className="jv-form" onSubmit={step === STEPS.length - 1 ? send : next}>
            {step === 0 && (
              <>
                <label className="jv-form-wide">
                  Name
                  <input value={a.name} onChange={(e) => set('name', e.target.value)} required maxLength={120} autoComplete="name" />
                </label>
                <label className="jv-form-wide">
                  Email
                  <input value={claims.email} readOnly aria-describedby="jv-email-note" />
                </label>
                <p className="jv-fine jv-form-wide" id="jv-email-note">
                  From your Google account.
                </p>
                <label className="jv-form-wide">
                  Your role (optional)
                  <input value={a.role} onChange={(e) => set('role', e.target.value)} maxLength={120} autoComplete="organization-title" />
                </label>
              </>
            )}
            {step === 1 && (
              <>
                <label className="jv-form-wide">
                  Company
                  <input value={a.company} onChange={(e) => set('company', e.target.value)} required maxLength={160} autoComplete="organization" />
                </label>
                <label className="jv-form-wide">
                  Website (optional)
                  <input value={a.company_site} onChange={(e) => set('company_site', e.target.value)} maxLength={200} autoComplete="url" />
                </label>
              </>
            )}
            {step === 2 && (
              <>
                <label>
                  Monthly email volume
                  <select value={a.volume} onChange={(e) => set('volume', e.target.value)} required>
                    <option value="" disabled>
                      Choose one
                    </option>
                    {VOLUME_OPTIONS.map((o) => (
                      <option key={o}>{o}</option>
                    ))}
                  </select>
                </label>
                <label>
                  Primary use case
                  <select value={a.use_case} onChange={(e) => set('use_case', e.target.value)} required>
                    <option value="" disabled>
                      Choose one
                    </option>
                    {USE_CASES.map((o) => (
                      <option key={o}>{o}</option>
                    ))}
                  </select>
                </label>
                <label className="jv-form-wide">
                  Current platform (optional)
                  <input value={a.current_platform} onChange={(e) => set('current_platform', e.target.value)} maxLength={120} />
                </label>
                <label className="jv-form-wide">
                  Anything else (optional)
                  <textarea rows={3} value={a.notes} onChange={(e) => set('notes', e.target.value)} maxLength={2000} />
                </label>
                <label className="jv-form-check jv-form-wide">
                  <input type="checkbox" checked={a.marketing_opt_in} onChange={(e) => set('marketing_opt_in', e.target.checked)} />
                  Also send me product updates
                </label>
                <Honeypot inputRef={honeypot} />
              </>
            )}
            {status === 'error' && (
              <p className="jv-form-error jv-form-wide" role="alert">
                {SUBMIT_ERROR}
              </p>
            )}
            <div className="jv-form-nav jv-form-wide">
              {step > 0 && (
                <button
                  type="button"
                  className="jv-btn jv-btn--ghost"
                  onClick={() => {
                    moved.current = true;
                    setStep((s) => s - 1);
                  }}
                >
                  Back
                </button>
              )}
              <button type="submit" className="jv-btn jv-btn--primary" disabled={status === 'sending'}>
                {step < STEPS.length - 1 ? 'Next' : status === 'sending' ? 'Sending…' : 'Submit'}
              </button>
            </div>
          </form>
        </div>
      )}
    </section>
  );
};
