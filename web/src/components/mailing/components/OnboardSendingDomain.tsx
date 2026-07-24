import React, { useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faRocket,
  faSpinner,
  faCheck,
  faExclamationTriangle,
  faArrowRight,
  faServer,
  faGlobe,
  faLayerGroup,
} from '@fortawesome/free-solid-svg-icons';
import apiFetch from '../shared/apiFetch';
import './OnboardSendingDomain.css';

// ============================================================================
// Onboard Sending Domain — the one-action self-service surface.
//
// Calls POST /api/mailing/sending-domains/onboard which, in one org-scoped
// transaction, creates:
//   1. the SES tenant sending profile (vendor_type='pmta', via_ses=true,
//      config-set + tenant + tracking domain),
//   2. the owned-domain registration (brand-root / suppression / image-host),
//   3. the segment-registry family for the domain.
// AWS-side prerequisites (SES identity, DKIM/MAILFROM DNS, tracking CNAME)
// must already exist — this screen wires the PLATFORM side and then hands off
// to Verify on the profile to confirm the DNS/identity truthfully.
// ============================================================================

interface OnboardForm {
  domain: string;
  tracking_domain: string;
  ses_configuration_set: string;
  ses_tenant_name: string;
  from_name: string;
  from_email: string;
}

interface OnboardResultView {
  raw: any;
  profileId?: string;
  profileName?: string;
  ownedDomain?: string;
  segmentFamily?: string;
  warnings: string[];
}

// The backend response shape is parsed defensively: known keys render as
// labeled artifacts, everything else stays inspectable in the raw block.
function parseOnboardResponse(data: any): OnboardResultView {
  const out: OnboardResultView = { raw: data, warnings: [] };
  if (data && typeof data === 'object') {
    out.profileId =
      (typeof data.profile_id === 'string' && data.profile_id) ||
      (data.profile && typeof data.profile.id === 'string' ? data.profile.id : undefined);
    out.profileName =
      (typeof data.profile_name === 'string' && data.profile_name) ||
      (data.profile && typeof data.profile.name === 'string' ? data.profile.name : undefined);
    out.ownedDomain =
      (typeof data.owned_domain === 'string' && data.owned_domain) ||
      (typeof data.domain === 'string' ? data.domain : undefined);
    out.segmentFamily =
      (typeof data.segment_family === 'string' && data.segment_family) ||
      (typeof data.segment_registry === 'string' ? data.segment_registry : undefined);
    if (Array.isArray(data.warnings)) {
      out.warnings = data.warnings.filter((w: any) => typeof w === 'string');
    }
  }
  return out;
}

// First label of the apex ("wcl-heloc.com" -> "wcl-heloc") — the default slug
// for config-set/tenant. Editable: it MUST match the names created on the AWS
// side, which may differ (e.g. hyphens stripped).
function slugFromDomain(domain: string): string {
  const apex = domain.trim().toLowerCase();
  const firstLabel = apex.split('.')[0] || '';
  return firstLabel;
}

const DOMAIN_RE = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$/i;

const EMPTY: OnboardForm = {
  domain: '',
  tracking_domain: '',
  ses_configuration_set: '',
  ses_tenant_name: '',
  from_name: '',
  from_email: '',
};

interface Props {
  // Jump to another Domain Center view after success (e.g. 'sending').
  onNavigateToProfiles?: () => void;
}

export const OnboardSendingDomain: React.FC<Props> = ({ onNavigateToProfiles }) => {
  const [form, setForm] = useState<OnboardForm>({ ...EMPTY });
  // Tracks which derived fields the user has hand-edited so typing the domain
  // keeps prefilling the rest without clobbering deliberate overrides.
  const [touched, setTouched] = useState<Record<string, boolean>>({});
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string | null>(null);
  const [result, setResult] = useState<OnboardResultView | null>(null);

  const setField = (key: keyof OnboardForm, value: string, isDerived = false) => {
    setForm(prev => ({ ...prev, [key]: value }));
    if (!isDerived) setTouched(prev => ({ ...prev, [key]: true }));
  };

  const handleDomainChange = (value: string) => {
    setForm(prev => {
      const next: OnboardForm = { ...prev, domain: value };
      const apex = value.trim().toLowerCase();
      const slug = slugFromDomain(apex);
      if (!touched.tracking_domain) next.tracking_domain = apex ? `t.em.${apex}` : '';
      if (!touched.ses_configuration_set) next.ses_configuration_set = slug;
      if (!touched.ses_tenant_name) next.ses_tenant_name = slug;
      if (!touched.from_email) next.from_email = apex ? `hello@em.${apex}` : '';
      return next;
    });
  };

  const validate = (): Record<string, string> => {
    const errs: Record<string, string> = {};
    const apex = form.domain.trim().toLowerCase();
    if (!apex) errs.domain = 'Apex domain is required (e.g. wcl-heloc.com).';
    else if (!DOMAIN_RE.test(apex)) errs.domain = 'Not a valid domain name.';
    if (!form.tracking_domain.trim()) errs.tracking_domain = 'Tracking domain is required — SES sends click-wrap through it.';
    else if (!DOMAIN_RE.test(form.tracking_domain.trim())) errs.tracking_domain = 'Not a valid domain name.';
    if (!form.ses_configuration_set.trim()) errs.ses_configuration_set = 'Required — must match the SES configuration set created in AWS.';
    if (!form.ses_tenant_name.trim()) errs.ses_tenant_name = 'Required — must match the SES tenant created in AWS.';
    if (!form.from_name.trim()) errs.from_name = 'From name is required.';
    if (!form.from_email.trim()) errs.from_email = 'From email is required.';
    else if (!/^[^@\s]+@[^@\s]+\.[^@\s]+$/.test(form.from_email.trim())) errs.from_email = 'Not a valid email address.';
    return errs;
  };

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    const errs = validate();
    setErrors(errs);
    if (Object.keys(errs).length > 0) return;

    setSubmitting(true);
    setSubmitError(null);
    try {
      const res = await apiFetch('/api/mailing/sending-domains/onboard', {
        method: 'POST',
        body: JSON.stringify({
          domain: form.domain.trim().toLowerCase(),
          tracking_domain: form.tracking_domain.trim().toLowerCase(),
          ses_configuration_set: form.ses_configuration_set.trim(),
          ses_tenant_name: form.ses_tenant_name.trim(),
          from_name: form.from_name.trim(),
          from_email: form.from_email.trim().toLowerCase(),
        }),
      });
      const data = await res.json().catch(() => null);
      if (!res.ok) {
        if (res.status === 404) {
          throw new Error(
            'The onboard endpoint is not available on this server build (POST /api/mailing/sending-domains/onboard returned 404). The backend change may not be deployed yet.'
          );
        }
        throw new Error(data?.error || `Onboarding failed (HTTP ${res.status})`);
      }
      setResult(parseOnboardResponse(data));
    } catch (err) {
      setSubmitError(err instanceof Error ? err.message : 'Onboarding failed');
    } finally {
      setSubmitting(false);
    }
  };

  const resetAll = () => {
    setForm({ ...EMPTY });
    setTouched({});
    setErrors({});
    setSubmitError(null);
    setResult(null);
  };

  // ---------------------------------------------------------------- success
  if (result) {
    return (
      <div className="onboard-domain">
        <div className="onboard-header">
          <h1><FontAwesomeIcon icon={faCheck} style={{ color: '#00b894' }} /> Sending Domain Onboarded</h1>
          <p className="subtitle">Created in one transaction for {form.domain.trim().toLowerCase()}</p>
        </div>

        <div className="onboard-success-panel">
          <div className="onboard-artifact">
            <FontAwesomeIcon icon={faServer} className="artifact-icon ok" />
            <div>
              <strong>SES sending profile</strong>
              <small>
                {result.profileName || `pmta · via_ses · config-set ${form.ses_configuration_set} · tenant ${form.ses_tenant_name}`}
                {result.profileId && <> · id <code>{result.profileId}</code></>}
              </small>
            </div>
          </div>
          <div className="onboard-artifact">
            <FontAwesomeIcon icon={faGlobe} className="artifact-icon ok" />
            <div>
              <strong>Owned domain registered</strong>
              <small>{result.ownedDomain || form.domain.trim().toLowerCase()} — brand root, suppression scope, image host</small>
            </div>
          </div>
          <div className="onboard-artifact">
            <FontAwesomeIcon icon={faLayerGroup} className="artifact-icon ok" />
            <div>
              <strong>Segment-registry family</strong>
              <small>{result.segmentFamily || `Engagement segment family for ${form.domain.trim().toLowerCase()}`}</small>
            </div>
          </div>

          {result.warnings.length > 0 && (
            <div className="onboard-warnings">
              {result.warnings.map((w, i) => (
                <div key={i}><FontAwesomeIcon icon={faExclamationTriangle} /> {w}</div>
              ))}
            </div>
          )}

          <details className="onboard-raw">
            <summary>Server response</summary>
            <pre>{JSON.stringify(result.raw, null, 2)}</pre>
          </details>

          <div className="onboard-next">
            <strong>Next:</strong> run <em>Verify</em> on the new profile to confirm SPF / DKIM / DMARC / SES identity
            before the first send.
          </div>

          <div className="onboard-actions">
            {onNavigateToProfiles && (
              <button className="onboard-primary-btn" onClick={onNavigateToProfiles}>
                View Sending Profiles <FontAwesomeIcon icon={faArrowRight} />
              </button>
            )}
            <button className="onboard-secondary-btn" onClick={resetAll}>
              Onboard another domain
            </button>
          </div>
        </div>
      </div>
    );
  }

  // ------------------------------------------------------------------- form
  return (
    <div className="onboard-domain">
      <div className="onboard-header">
        <h1><FontAwesomeIcon icon={faRocket} /> Onboard Sending Domain</h1>
        <p className="subtitle">
          One action creates the SES tenant profile, the owned-domain registration, and the segment-registry
          family. SES identity + DNS (DKIM, MAILFROM, tracking CNAME) must already be provisioned in AWS.
        </p>
      </div>

      <form className="onboard-form" onSubmit={handleSubmit} noValidate>
        <div className="onboard-form-section">
          <h3>Domain</h3>
          <div className="onboard-form-row">
            <div className="onboard-form-group">
              <label>Apex Domain *</label>
              <input
                type="text"
                value={form.domain}
                onChange={e => handleDomainChange(e.target.value)}
                placeholder="wcl-heloc.com"
                className={errors.domain ? 'input-error' : ''}
                autoFocus
              />
              {errors.domain
                ? <span className="field-error">{errors.domain}</span>
                : <span className="field-hint">The brand apex. Sending domain and defaults derive from it (em.&lt;apex&gt;).</span>}
            </div>
            <div className="onboard-form-group">
              <label>Tracking Domain *</label>
              <input
                type="text"
                value={form.tracking_domain}
                onChange={e => setField('tracking_domain', e.target.value)}
                placeholder="t.em.wcl-heloc.com"
                className={errors.tracking_domain ? 'input-error' : ''}
              />
              {errors.tracking_domain
                ? <span className="field-error">{errors.tracking_domain}</span>
                : <span className="field-hint">Open pixel + click-wrap base (t.em.&lt;apex&gt; convention).</span>}
            </div>
          </div>
        </div>

        <div className="onboard-form-section">
          <h3>SES Tenant Routing</h3>
          <div className="onboard-form-row">
            <div className="onboard-form-group">
              <label>SES Configuration Set *</label>
              <input
                type="text"
                value={form.ses_configuration_set}
                onChange={e => setField('ses_configuration_set', e.target.value)}
                placeholder="wcl-heloc"
                className={errors.ses_configuration_set ? 'input-error' : ''}
              />
              {errors.ses_configuration_set
                ? <span className="field-error">{errors.ses_configuration_set}</span>
                : <span className="field-hint">Must match the configuration set name created on the AWS side.</span>}
            </div>
            <div className="onboard-form-group">
              <label>SES Tenant Name *</label>
              <input
                type="text"
                value={form.ses_tenant_name}
                onChange={e => setField('ses_tenant_name', e.target.value)}
                placeholder="wcl-heloc"
                className={errors.ses_tenant_name ? 'input-error' : ''}
              />
              {errors.ses_tenant_name
                ? <span className="field-error">{errors.ses_tenant_name}</span>
                : <span className="field-hint">Must match the SES tenant name created on the AWS side.</span>}
            </div>
          </div>
        </div>

        <div className="onboard-form-section">
          <h3>Sender Identity</h3>
          <div className="onboard-form-row">
            <div className="onboard-form-group">
              <label>From Name *</label>
              <input
                type="text"
                value={form.from_name}
                onChange={e => setField('from_name', e.target.value)}
                placeholder="e.g. Jamie @ WCL HELOC"
                className={errors.from_name ? 'input-error' : ''}
              />
              {errors.from_name
                ? <span className="field-error">{errors.from_name}</span>
                : <span className="field-hint">The consistent friendly-from persona for this brand — it never rotates.</span>}
            </div>
            <div className="onboard-form-group">
              <label>From Email *</label>
              <input
                type="email"
                value={form.from_email}
                onChange={e => setField('from_email', e.target.value)}
                placeholder="hello@em.wcl-heloc.com"
                className={errors.from_email ? 'input-error' : ''}
              />
              {errors.from_email
                ? <span className="field-error">{errors.from_email}</span>
                : <span className="field-hint">Convention: hello@em.&lt;apex&gt;.</span>}
            </div>
          </div>
        </div>

        {submitError && (
          <div className="onboard-error-banner">
            <FontAwesomeIcon icon={faExclamationTriangle} /> {submitError}
          </div>
        )}

        <div className="onboard-form-actions">
          <button type="submit" className="onboard-primary-btn" disabled={submitting}>
            {submitting
              ? <><FontAwesomeIcon icon={faSpinner} spin /> Onboarding…</>
              : <><FontAwesomeIcon icon={faRocket} /> Onboard Domain</>}
          </button>
          <span className="onboard-actions-hint">
            Creates the profile + registry rows in this organization. Nothing is mailed.
          </span>
        </div>
      </form>
    </div>
  );
};

export default OnboardSendingDomain;
