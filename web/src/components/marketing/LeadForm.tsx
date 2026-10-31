import React, { useCallback, useEffect, useRef, useState } from 'react';
import { PageLink, useMarketingNav } from './parts';

// Every public form posts through this file. Success is only ever shown after
// the server answers 2xx; on any failure the fields stay as typed.

export const SUBMIT_ERROR = 'We couldn’t submit your request. Please try again or contact us.';

const MIN_TOKEN_AGE_MS = 3200; // the server drops anything younger than 3s

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

const fetchFormToken = async (): Promise<string> => {
  const res = await fetch('/api/public/form-token', { credentials: 'omit' });
  if (!res.ok) throw new Error(`token ${res.status}`);
  return (await res.json()).token as string;
};

export type SubmitResult = 'ok' | 'unauthorized' | 'error';

// Holds the form token and handles the two cases a person can hit: submitting
// quickly after the page loaded, and submitting from a tab left open too long.
export const useFormSubmit = () => {
  const token = useRef<{ value: string; at: number } | null>(null);

  const refresh = useCallback(async () => {
    token.current = { value: await fetchFormToken(), at: Date.now() };
  }, []);

  useEffect(() => {
    refresh().catch(() => {
      token.current = null;
    });
  }, [refresh]);

  return useCallback(
    async (path: string, body: Record<string, unknown>): Promise<SubmitResult> => {
      try {
        for (let attempt = 0; attempt < 2; attempt++) {
          if (!token.current) await refresh();
          const age = Date.now() - (token.current as { at: number }).at;
          if (age < MIN_TOKEN_AGE_MS) await sleep(MIN_TOKEN_AGE_MS - age);
          const res = await fetch(path, {
            method: 'POST',
            credentials: 'omit',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ ...body, form_token: token.current?.value }),
          });
          if (res.ok) return 'ok';
          if (res.status === 401) return 'unauthorized';
          if (res.status !== 409) return 'error';
          token.current = null;
        }
      } catch {
        return 'error';
      }
      return 'error';
    },
    [refresh],
  );
};

// Off-screen, not display:none: simple bots fill it, people never reach it.
export const Honeypot: React.FC<{ inputRef: React.RefObject<HTMLInputElement> }> = ({ inputRef }) => (
  <div className="jv-hp" aria-hidden="true">
    <label>
      Website
      <input ref={inputRef} name="website" type="text" tabIndex={-1} autoComplete="off" />
    </label>
  </div>
);

export const VOLUME_OPTIONS = ['Just starting', 'Under 10,000', '10,000 to 100,000', '100,000 to 1 million', 'Over 1 million'];
export const USE_CASES = ['Newsletters', 'Promotions', 'Customer follow-ups', 'Welcome emails', 'Something else'];

export type FieldDef =
  | { name: string; label: string; kind: 'text' | 'email'; required?: boolean; autoComplete?: string; wide?: boolean }
  | { name: string; label: string; kind: 'select'; options: string[]; required?: boolean; wide?: boolean }
  | { name: string; label: string; kind: 'textarea'; required?: boolean };

export const Field: React.FC<{ field: FieldDef; autoFocus?: boolean }> = ({ field: f }) => {
  const label = `${f.label}${f.required ? '' : ' (optional)'}`;
  if (f.kind === 'textarea') {
    return (
      <label className="jv-form-wide">
        {label}
        <textarea name={f.name} rows={3} required={f.required} maxLength={2000} />
      </label>
    );
  }
  if (f.kind === 'select') {
    return (
      <label className={f.wide ? 'jv-form-wide' : undefined}>
        {label}
        <select name={f.name} required={f.required} defaultValue="">
          <option value="" disabled>
            Choose one
          </option>
          {f.options.map((o) => (
            <option key={o}>{o}</option>
          ))}
        </select>
      </label>
    );
  }
  return (
    <label className={f.wide ? 'jv-form-wide' : undefined}>
      {label}
      <input name={f.name} type={f.kind} required={f.required} autoComplete={f.autoComplete} maxLength={f.kind === 'email' ? 254 : 160} />
    </label>
  );
};

interface LeadFormProps {
  kind: 'early_access' | 'demo' | 'contact' | 'api_access';
  fields: FieldDef[];
  submitLabel: string;
  success: string;
  // Sent as-is when the form has no use_case field of its own.
  fixedUseCase?: string;
  compact?: boolean;
  offerUpdates?: boolean;
}

export const LeadForm: React.FC<LeadFormProps> = ({ kind, fields, submitLabel, success, fixedUseCase, compact, offerUpdates }) => {
  const { fromPath } = useMarketingNav();
  const submit = useFormSubmit();
  const honeypot = useRef<HTMLInputElement>(null);
  const [state, setState] = useState<'idle' | 'sending' | 'sent' | 'error'>('idle');

  const onSubmit = async (e: React.FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    if (state === 'sending') return;
    setState('sending');
    const data = new FormData(e.currentTarget);
    const body: Record<string, unknown> = { kind, source_page: fromPath || window.location.pathname, website: honeypot.current?.value ?? '' };
    fields.forEach((f) => {
      body[f.name] = String(data.get(f.name) ?? '');
    });
    if (fixedUseCase) body.use_case = fixedUseCase;
    if (offerUpdates) body.marketing_opt_in = data.get('marketing_opt_in') === 'on';
    setState((await submit('/api/public/leads', body)) === 'ok' ? 'sent' : 'error');
  };

  if (state === 'sent') {
    return (
      <p className="jv-form-ok" role="status">
        {success}
      </p>
    );
  }

  return (
    <form className={`jv-form${compact ? ' jv-form--compact' : ''}`} onSubmit={onSubmit}>
      {fields.map((f) => (
        <Field key={f.name} field={f} />
      ))}
      <Honeypot inputRef={honeypot} />
      {offerUpdates && (
        <label className="jv-form-check jv-form-wide">
          <input type="checkbox" name="marketing_opt_in" />
          Also send me product updates
        </label>
      )}
      {state === 'error' && (
        <p className="jv-form-error jv-form-wide" role="alert">
          {SUBMIT_ERROR}
        </p>
      )}
      <button type="submit" className="jv-btn jv-btn--primary jv-form-wide" disabled={state === 'sending'}>
        {state === 'sending' ? 'Sending…' : submitLabel}
      </button>
      <p className="jv-fine jv-form-wide">
        We use these details to reply to you. See our <PageLink to="privacy">privacy notice</PageLink>.
      </p>
    </form>
  );
};
