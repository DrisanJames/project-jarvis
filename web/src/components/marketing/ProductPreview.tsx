import React from 'react';
import { JarvisMark } from './JarvisLogo';

// Product views for the site, built in React from made-up data. Each one
// carries a visible Example label, so nothing reads as a real account.

const Frame: React.FC<{ label: string; title: string; children: React.ReactNode }> = ({ label, title, children }) => (
  <figure className="jv-pv">
    <figcaption className="jv-pv-bar">
      <span className="jv-pv-title">{title}</span>
      <span className="jv-pv-tag">{label}</span>
    </figcaption>
    <div className="jv-pv-body">{children}</div>
  </figure>
);

export const CampaignPreview: React.FC = () => (
  <Frame label="Example campaign" title="Spring update newsletter">
    <div className="jv-pv-campaign">
      <div className="jv-pv-draft">
        <dl className="jv-pv-fields">
          <div>
            <dt>Status</dt>
            <dd>
              <span className="jv-pv-status">Draft</span>
            </dd>
          </div>
          <div>
            <dt>Audience</dt>
            <dd>Monthly readers · 4,200 contacts</dd>
          </div>
          <div>
            <dt>Subject</dt>
            <dd>Three things worth reading this spring</dd>
          </div>
        </dl>
        <div className="jv-pv-copy">
          <p>Hi Sam,</p>
          <p>Spring is busy, so we kept this one short. Here are the three stories our readers liked most this month.</p>
        </div>
      </div>
      <div className="jv-pv-ai">
        <div className="jv-pv-ai-head">
          <JarvisMark size={30} title="Jarvis" animated />
          <strong>Suggestion</strong>
        </div>
        <p>Here are two more subject lines. Pick one or keep yours.</p>
        <ul>
          <li>Your spring reading list is here</li>
          <li>Three short reads for a busy week</li>
        </ul>
      </div>
    </div>
  </Frame>
);

const GROUPS = [
  { name: 'New this month', count: '640', note: 'Joined in the last 30 days' },
  { name: 'Monthly readers', count: '4,200', note: 'Opened or clicked recently' },
  { name: 'Unsubscribed', count: '310', note: 'Left out of every send' },
];

export const AudiencePreview: React.FC = () => (
  <Frame label="Example audience" title="Audiences">
    <table className="jv-pv-table">
      <thead>
        <tr>
          <th scope="col">Group</th>
          <th scope="col">Contacts</th>
          <th scope="col">Rule</th>
        </tr>
      </thead>
      <tbody>
        {GROUPS.map((g) => (
          <tr key={g.name}>
            <th scope="row">{g.name}</th>
            <td>{g.count}</td>
            <td>{g.note}</td>
          </tr>
        ))}
      </tbody>
    </table>
  </Frame>
);

const JOURNEY = [
  { label: 'Joins the list', kind: 'Start' },
  { label: 'Welcome email', kind: 'Send' },
  { label: 'Wait 2 days', kind: 'Delay' },
  { label: 'Best of the newsletter', kind: 'Send' },
];

export const AutomationPreview: React.FC = () => (
  <Frame label="Example journey" title="Welcome series">
    <ol className="jv-pv-journey">
      {JOURNEY.map((s) => (
        <li key={s.label}>
          <span>{s.kind}</span>
          {s.label}
        </li>
      ))}
    </ol>
  </Frame>
);

const REPORT = [
  { label: 'Delivered', value: '4,118', width: 98, cls: '' },
  { label: 'Recorded opens', value: '1,482', width: 36, cls: 'jv-pv-bar-fill--cyan' },
  { label: 'Clicks', value: '214', width: 12, cls: 'jv-pv-bar-fill--pink' },
];

export const ReportPreview: React.FC = () => (
  <Frame label="Example report" title="Spring update results">
    <div className="jv-pv-report">
      {REPORT.map((r) => (
        <div key={r.label} className="jv-pv-metric">
          <span>{r.label}</span>
          <strong>{r.value}</strong>
          <div className="jv-pv-track">
            <div className={`jv-pv-bar-fill ${r.cls}`} style={{ width: `${r.width}%` }} />
          </div>
        </div>
      ))}
      <p className="jv-pv-summary">
        <JarvisMark size={24} title="Jarvis" /> Clicks were highest on the first story. Worth testing it at the top again.
      </p>
    </div>
  </Frame>
);
