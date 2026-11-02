// Account.tsx — profile, company, subscription, users and provider keys, over
// /api/mailing/account/* (internal/api/account_*.go).
//
// Rules this screen honours:
//   - It displays what the API returns and computes nothing. Roles decide the
//     controls here, but the server refuses every write on its own.
//   - Loading, error, 404 and empty are different displays. A missing number
//     shows as "Not available", never as zero.
//   - A key value lives only in the Replace form's state and is cleared on
//     save, cancel and close. The API never sends one back.

import React, { useCallback, useEffect, useState } from 'react'
import { apiFetch } from '../shared/apiFetch'
import { colors, tableStyle, thStyle, tdStyle, btnStyle } from '../shared/theme'
import { Panel, SectionHeader, Stat, SectionError, EmptyState, Pill, ProgressBar } from '../shared/ui'
import { SubNav } from '../shared/SubNav'
import { useToast } from '../shared/ToastSystem'
import { useAuth } from '../../../contexts/AuthContext'
import './Account.css'

export type AccountTab = 'profile' | 'company' | 'subscription' | 'users' | 'keys'

const TABS: { key: AccountTab; label: string }[] = [
  { key: 'profile', label: 'Profile' },
  { key: 'company', label: 'Company' },
  { key: 'subscription', label: 'Subscription' },
  { key: 'users', label: 'Users' },
  { key: 'keys', label: 'API Keys' },
]

const ROLES = ['owner', 'admin', 'user', 'viewer'] as const

const ROLE_LABELS: Record<string, string> = {
  owner: 'Owner', admin: 'Admin', user: 'User', viewer: 'Viewer',
}

// Off-box agents read Secrets Manager, which the server has no access to.
const AGENT_KEY_STORES = [
  { name: 'Scheduling, data and reporting agents', where: 'jarvis-agents/*' },
  { name: 'Warmup agent', where: 'jarvis-warmup-agent/env' },
]

interface Profile {
  id: string
  email: string
  name: string
  picture: string
  role: string
  status: string
  last_sign_in: string | null
  can_manage: boolean
}

interface Company {
  name: string
  slug: string
  plan: string
  status: string
  daily_limit: number
  monthly_limit: number
}

interface Subscription {
  plan: string
  status: string
  daily_limit: number
  monthly_limit: number
  month: string
  sent_this_month: number | null
  sent_as_of: string | null
  billing_connected: boolean
}

interface AccountUser {
  id: string
  email: string
  name: string
  picture: string
  role: string
  status: string
  last_sign_in: string | null
}

interface ProviderKey {
  name: string
  label: string
  in_use_by: string
  source: string
  last4: string
  configured: boolean
  updated_by: string
  updated_at: string | null
  applies: string
  applies_note: string
  reverts_on_restart: boolean
  warning: string
}

class AccountError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

function fallbackMessage(status: number): string {
  if (status === 401) return 'Your session ended. Sign in again.'
  if (status === 403) return "You don't have permission to do this."
  if (status === 404) return "This isn't available on the server yet."
  return 'Something went wrong. Please try again.'
}

async function accountRequest<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await apiFetch(`/api/mailing/account${path}`, init)
  const body = await res.json().catch(() => null)
  if (!res.ok) throw new AccountError(res.status, body?.error || fallbackMessage(res.status))
  return body as T
}

const errorMessage = (e: unknown) => (e instanceof Error ? e.message : 'Something went wrong. Please try again.')

type Load<T> =
  | { state: 'loading' }
  | { state: 'ready'; data: T }
  | { state: 'error'; message: string }

function useAccountData<T>(path: string) {
  const [load, setLoad] = useState<Load<T>>({ state: 'loading' })
  const fetchIt = useCallback(() => {
    let live = true
    setLoad({ state: 'loading' })
    accountRequest<T>(path)
      .then(data => { if (live) setLoad({ state: 'ready', data }) })
      .catch(e => { if (live) setLoad({ state: 'error', message: errorMessage(e) }) })
    return () => { live = false }
  }, [path])
  useEffect(() => fetchIt(), [fetchIt])
  const setData = useCallback((data: T) => setLoad({ state: 'ready', data }), [])
  return { load, reload: fetchIt, setData }
}

function denverTime(iso: string | null | undefined): string {
  if (!iso) return 'Not yet'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return 'Not available'
  return `${d.toLocaleString('en-US', { timeZone: 'America/Denver', dateStyle: 'medium', timeStyle: 'short' })} MT`
}

const num = (n: number) => n.toLocaleString('en-US')
const titleCase = (s: string) => (s ? s[0].toUpperCase() + s.slice(1) : 'Not set')
const statusColor = (status: string) => (status === 'active' ? colors.success : colors.idle)

function monthLabel(month: string): string {
  const [y, m] = month.split('-').map(Number)
  if (!y || !m) return month
  return new Date(Date.UTC(y, m - 1, 1)).toLocaleString('en-US', { month: 'long', year: 'numeric', timeZone: 'UTC' })
}

const Avatar: React.FC<{ src: string; name: string }> = ({ src, name }) => {
  const [broken, setBroken] = useState(false)
  useEffect(() => setBroken(false), [src])
  if (src && !broken) {
    return <img className="account__photo" src={src} alt="" referrerPolicy="no-referrer" onError={() => setBroken(true)} />
  }
  return <div className="account__photo account__initials" aria-hidden="true">{(name || '?')[0].toUpperCase()}</div>
}

const Loading: React.FC = () => <div className="account__loading">Loading…</div>

// ─── Profile ────────────────────────────────────────────────────────────────

const ProfileView: React.FC<{ profile: Profile }> = ({ profile }) => {
  const { logout } = useAuth()
  return (
    <Panel>
      <div className="account__person">
        <Avatar src={profile.picture} name={profile.name || profile.email} />
        <div className="account__who">
          <div className="account__name">{profile.name || profile.email}</div>
          <div className="account__email">{profile.email}</div>
        </div>
        <button type="button" className="account__action" style={btnStyle} onClick={logout}>Sign out</button>
      </div>
      <dl className="account__facts">
        <div><dt>Role</dt><dd>{ROLE_LABELS[profile.role] ?? profile.role}</dd></div>
        <div><dt>Last sign-in</dt><dd>{denverTime(profile.last_sign_in)}</dd></div>
      </dl>
    </Panel>
  )
}

// ─── Company ────────────────────────────────────────────────────────────────

const CompanyView: React.FC<{ canManage: boolean }> = ({ canManage }) => {
  const { load, reload, setData } = useAccountData<Company>('/company')
  const { addToast } = useToast()
  const [name, setName] = useState('')
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    if (load.state === 'ready') setName(load.data.name)
  }, [load])

  if (load.state === 'loading') return <Panel><Loading /></Panel>
  if (load.state === 'error') return <SectionError label="Company" error={load.message} onRetry={reload} />
  const company = load.data

  const save = async (e: React.FormEvent) => {
    e.preventDefault()
    setSaving(true)
    setError('')
    try {
      const updated = await accountRequest<Company>('/company', { method: 'PUT', body: JSON.stringify({ name }) })
      setData(updated)
      addToast({ type: 'success', title: 'Company name saved' })
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setSaving(false)
    }
  }

  return (
    <Panel>
      <SectionHeader title="Company" />
      {canManage ? (
        <form className="account__form" onSubmit={save}>
          <label>
            Company name
            <input value={name} maxLength={255} onChange={e => setName(e.target.value)} />
          </label>
          <button
            type="submit"
            className="account__action"
            style={btnStyle}
            disabled={saving || !name.trim() || name.trim() === company.name}
          >
            {saving ? 'Saving…' : 'Save'}
          </button>
        </form>
      ) : (
        <p className="account__name">{company.name}</p>
      )}
      {error && <p className="account__error">{error}</p>}
      <dl className="account__facts">
        <div><dt>Short name</dt><dd>{company.slug}</dd></div>
        <div><dt>Plan</dt><dd>{titleCase(company.plan)}</dd></div>
        <div><dt>Status</dt><dd>{titleCase(company.status)}</dd></div>
        <div><dt>Daily limit</dt><dd>{num(company.daily_limit)} emails</dd></div>
        <div><dt>Monthly limit</dt><dd>{num(company.monthly_limit)} emails</dd></div>
      </dl>
    </Panel>
  )
}

// ─── Subscription ───────────────────────────────────────────────────────────

const SubscriptionView: React.FC = () => {
  const { load, reload } = useAccountData<Subscription>('/subscription')
  if (load.state === 'loading') return <Panel><Loading /></Panel>
  if (load.state === 'error') return <SectionError label="Subscription" error={load.message} onRetry={reload} />
  const sub = load.data
  const sent = sub.sent_this_month
  const usedShare = sent != null && sub.monthly_limit > 0 ? sent / sub.monthly_limit : null

  return (
    <div className="account__stack">
      <Panel>
        <SectionHeader title="Plan" right={<Pill color={statusColor(sub.status)}>{titleCase(sub.status)}</Pill>} />
        <div className="account__stats">
          <Stat label="Plan" value={titleCase(sub.plan)} />
          <Stat label="Daily limit" value={num(sub.daily_limit)} sub="emails" />
          <Stat label="Monthly limit" value={num(sub.monthly_limit)} sub="emails" />
        </div>
        {!sub.billing_connected && <p className="account__line">Billing isn't connected yet.</p>}
      </Panel>
      <Panel>
        <SectionHeader title={`Sent in ${monthLabel(sub.month)}`} />
        {sent == null ? (
          <p className="account__muted">This month's count isn't available right now.</p>
        ) : (
          <>
            <div className="account__stats">
              <Stat
                label="Sent"
                value={num(sent)}
                sub={usedShare != null ? `${Math.round(usedShare * 100)}% of the monthly limit` : undefined}
              />
            </div>
            {usedShare != null && <div style={{ marginTop: 12 }}><ProgressBar pct={usedShare} /></div>}
            <p className="account__line">
              Counted as of {denverTime(sub.sent_as_of)}. Limits are shown for reference. Sending doesn't stop at them.
            </p>
          </>
        )}
      </Panel>
    </div>
  )
}

// ─── Users ──────────────────────────────────────────────────────────────────

const AddUserForm: React.FC<{ callerIsOwner: boolean; onAdded: (u: AccountUser) => void }> = ({ callerIsOwner, onAdded }) => {
  const [email, setEmail] = useState('')
  const [name, setName] = useState('')
  const [role, setRole] = useState('user')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      const user = await accountRequest<AccountUser>('/users', {
        method: 'POST',
        body: JSON.stringify({ email, name, role }),
      })
      setEmail('')
      setName('')
      setRole('user')
      onAdded(user)
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Panel>
      <SectionHeader title="Add a user" />
      <form className="account__form" onSubmit={submit}>
        <label>
          Work email
          <input type="email" required value={email} onChange={e => setEmail(e.target.value)} autoComplete="off" />
        </label>
        <label>
          Name (optional)
          <input value={name} maxLength={255} onChange={e => setName(e.target.value)} autoComplete="off" />
        </label>
        <label className="account__narrow">
          Role
          <select value={role} onChange={e => setRole(e.target.value)}>
            {ROLES.filter(r => callerIsOwner || r !== 'owner').map(r => (
              <option key={r} value={r}>{ROLE_LABELS[r]}</option>
            ))}
          </select>
        </label>
        <button type="submit" className="account__action" style={btnStyle} disabled={busy || !email.trim()}>
          {busy ? 'Adding…' : 'Add user'}
        </button>
      </form>
      {error && <p className="account__error">{error}</p>}
    </Panel>
  )
}

const UsersView: React.FC<{ profile: Profile }> = ({ profile }) => {
  const { load, reload, setData } = useAccountData<{ users: AccountUser[] }>('/users')
  const { addToast } = useToast()
  const [busyId, setBusyId] = useState('')
  const canManage = profile.can_manage
  const callerIsOwner = profile.role === 'owner'

  if (load.state === 'loading') return <Panel><Loading /></Panel>
  if (load.state === 'error') return <SectionError label="Users" error={load.message} onRetry={reload} />
  const users = load.data.users

  const replaceUser = (u: AccountUser) =>
    setData({ users: users.map(x => (x.id === u.id ? u : x)) })

  const patch = async (u: AccountUser, change: { role?: string; status?: string }) => {
    setBusyId(u.id)
    try {
      replaceUser(await accountRequest<AccountUser>(`/users/${u.id}`, {
        method: 'PATCH',
        body: JSON.stringify(change),
      }))
    } catch (err) {
      addToast({ type: 'error', title: "Couldn't save that change", message: errorMessage(err) })
    } finally {
      setBusyId('')
    }
  }

  const disable = (u: AccountUser) => {
    if (window.confirm(`Turn off access for ${u.email}? They'll lose access within a minute.`)) {
      patch(u, { status: 'disabled' })
    }
  }

  return (
    <div className="account__stack">
      {canManage && (
        <AddUserForm
          callerIsOwner={callerIsOwner}
          onAdded={u => setData({ users: [...users, u].sort((a, b) => a.email.localeCompare(b.email)) })}
        />
      )}
      <Panel>
        <SectionHeader title="Users" />
        <p className="account__muted" style={{ marginBottom: 10 }}>
          Anyone with a company Google account can sign in. They appear here after they open this Account screen.
        </p>
        {users.length === 0 ? (
          <EmptyState title="No users yet" hint="People appear here after they open this Account screen." />
        ) : (
          <table className="account__table" style={tableStyle}>
            <thead>
              <tr>
                <th style={thStyle}>User</th>
                <th style={thStyle}>Role</th>
                <th style={thStyle}>Status</th>
                <th style={thStyle}>Last sign-in</th>
                {canManage && <th style={thStyle} aria-label="Actions" />}
              </tr>
            </thead>
            <tbody>
              {users.map(u => {
                const locked = !canManage || (u.role === 'owner' && !callerIsOwner)
                const busy = busyId === u.id
                return (
                  <tr key={u.id}>
                    <td style={tdStyle}>
                      <div className="account__userCell">
                        <Avatar src={u.picture} name={u.name || u.email} />
                        <div className="account__who">
                          <div>{u.name || u.email}</div>
                          {u.name && <div className="account__email">{u.email}</div>}
                        </div>
                      </div>
                    </td>
                    <td style={tdStyle} data-label="Role">
                      {locked ? (ROLE_LABELS[u.role] ?? u.role) : (
                        <select
                          aria-label={`Role for ${u.email}`}
                          value={u.role}
                          disabled={busy}
                          onChange={e => patch(u, { role: e.target.value })}
                        >
                          {ROLES.filter(r => callerIsOwner || r !== 'owner').map(r => (
                            <option key={r} value={r}>{ROLE_LABELS[r]}</option>
                          ))}
                        </select>
                      )}
                    </td>
                    <td style={tdStyle} data-label="Status">
                      <Pill color={statusColor(u.status)}>{titleCase(u.status)}</Pill>
                    </td>
                    <td style={tdStyle} data-label="Last sign-in">{denverTime(u.last_sign_in)}</td>
                    {canManage && (
                      <td style={tdStyle}>
                        <div className="account__rowActions">
                          {!locked && u.status === 'active' && (
                            <button type="button" style={btnStyle} disabled={busy} onClick={() => disable(u)}>Disable</button>
                          )}
                          {!locked && u.status !== 'active' && (
                            <button type="button" style={btnStyle} disabled={busy} onClick={() => patch(u, { status: 'active' })}>Enable</button>
                          )}
                        </div>
                      </td>
                    )}
                  </tr>
                )
              })}
            </tbody>
          </table>
        )}
      </Panel>
    </div>
  )
}

// ─── API keys ───────────────────────────────────────────────────────────────

const ReplaceKeyForm: React.FC<{
  providerKey: ProviderKey
  onSaved: (k: ProviderKey) => void
  onClose: () => void
}> = ({ providerKey, onSaved, onClose }) => {
  const [value, setValue] = useState('')
  const [confirming, setConfirming] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  const close = () => {
    setValue('')
    onClose()
  }

  const save = async () => {
    setBusy(true)
    setError('')
    try {
      const saved = await accountRequest<ProviderKey>(`/keys/${providerKey.name}`, {
        method: 'PUT',
        body: JSON.stringify({ value }),
      })
      setValue('')
      onSaved(saved)
    } catch (err) {
      setConfirming(false)
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  if (confirming) {
    return (
      <div className="account__replace">
        <p>
          Replace the {providerKey.label} key? It takes effect {providerKey.applies}.
        </p>
        <div className="account__form">
          <button type="button" className="account__action" style={btnStyle} disabled={busy} onClick={save}>
            {busy ? 'Saving…' : 'Confirm'}
          </button>
          <button type="button" className="account__action" style={btnStyle} disabled={busy} onClick={close}>Cancel</button>
        </div>
      </div>
    )
  }

  return (
    <form
      className="account__replace"
      onSubmit={e => { e.preventDefault(); setError(''); setConfirming(true) }}
    >
      <div className="account__form">
        <label>
          New {providerKey.label} key
          <input
            type="password"
            value={value}
            autoFocus
            autoComplete="new-password"
            spellCheck={false}
            onChange={e => setValue(e.target.value)}
          />
        </label>
        <button type="submit" className="account__action" style={btnStyle} disabled={!value}>Continue</button>
        <button type="button" className="account__action" style={btnStyle} onClick={close}>Cancel</button>
      </div>
      {error && <p className="account__error">{error}</p>}
    </form>
  )
}

const KeyRow: React.FC<{
  providerKey: ProviderKey
  canEdit: boolean
  onChanged: (k: ProviderKey) => void
}> = ({ providerKey: k, canEdit, onChanged }) => {
  const { addToast } = useToast()
  const [replacing, setReplacing] = useState(false)
  const [removing, setRemoving] = useState(false)

  const remove = async () => {
    if (!window.confirm(`Remove the saved ${k.label} key? The deployment setting comes back at the next deploy.`)) return
    setRemoving(true)
    try {
      onChanged(await accountRequest<ProviderKey>(`/keys/${k.name}`, { method: 'DELETE' }))
      addToast({ type: 'success', title: 'Saved key removed', message: 'The deployment setting comes back at the next deploy.' })
    } catch (err) {
      addToast({ type: 'error', title: "Couldn't remove the key", message: errorMessage(err) })
    } finally {
      setRemoving(false)
    }
  }

  return (
    <div className="account__key">
      <div className="account__keyHead">
        <div>
          <div className="account__keyName">{k.label}</div>
          <div className="account__keyUse">{k.in_use_by}</div>
        </div>
        {canEdit && !replacing && (
          <div className="account__rowActions">
            <button type="button" style={btnStyle} onClick={() => setReplacing(true)}>Replace</button>
            {k.source === 'saved here' && (
              <button type="button" style={btnStyle} disabled={removing} onClick={remove}>Use deployment setting</button>
            )}
          </div>
        )}
      </div>
      <div className="account__keyMeta">
        <span className="account__tail">
          {k.last4 ? `••••${k.last4}` : k.configured ? 'Set' : 'Not set'}
        </span>
        <span>Source: {k.source}</span>
        <span>Changes apply {k.applies}</span>
        {k.updated_by && <span>Saved by {k.updated_by}, {denverTime(k.updated_at)}</span>}
      </div>
      {k.applies_note && <p className="account__keyNote">{k.applies_note}</p>}
      {k.reverts_on_restart && (
        <p className="account__keyNote">The removed key keeps running until the next deploy.</p>
      )}
      {k.warning && <p className="account__keyWarn">{k.warning}</p>}
      {replacing && (
        <ReplaceKeyForm
          providerKey={k}
          onClose={() => setReplacing(false)}
          onSaved={saved => {
            setReplacing(false)
            onChanged(saved)
            addToast({ type: 'success', title: `${k.label} key saved`, message: `It takes effect ${saved.applies}.` })
          }}
        />
      )}
    </div>
  )
}

const KeysView: React.FC<{ canManage: boolean }> = ({ canManage }) => {
  const { load, reload, setData } = useAccountData<{ keys: ProviderKey[]; can_save: boolean }>('/keys')
  if (load.state === 'loading') return <Panel><Loading /></Panel>
  if (load.state === 'error') return <SectionError label="API keys" error={load.message} onRetry={reload} />
  const { keys, can_save: canSave } = load.data

  return (
    <div className="account__stack">
      <Panel>
        <SectionHeader title="Provider keys" />
        {!canManage && <p className="account__muted">Only an owner or admin can change these.</p>}
        {canManage && !canSave && <p className="account__muted">Keys can't be saved until sign-in is configured on this server.</p>}
        {keys.map(k => (
          <KeyRow
            key={k.name}
            providerKey={k}
            canEdit={canManage && canSave}
            onChanged={updated => setData({ ...load.data, keys: keys.map(x => (x.name === updated.name ? updated : x)) })}
          />
        ))}
      </Panel>
      <Panel>
        <SectionHeader title="Agent keys" />
        <p className="account__muted" style={{ marginBottom: 8 }}>
          Keys for the off-box agents live in AWS Secrets Manager and can't be changed here yet.
        </p>
        <ul className="account__agents">
          {AGENT_KEY_STORES.map(store => (
            <li key={store.where}>
              <strong>{store.name}</strong>
              <span className="account__tail">{store.where}</span>
            </li>
          ))}
        </ul>
      </Panel>
    </div>
  )
}

// ─── Page ───────────────────────────────────────────────────────────────────

const Account: React.FC<{ initial?: AccountTab }> = ({ initial = 'profile' }) => {
  const [tab, setTab] = useState<AccountTab>(initial)
  const { load, reload } = useAccountData<Profile>('/me')

  useEffect(() => setTab(initial), [initial])

  const profile = load.state === 'ready' ? load.data : null
  const canManage = profile?.can_manage ?? false

  return (
    <div className="account">
      <div className="account__head">
        <h2>Account</h2>
        <p>Your profile, company, subscription, users and API keys.</p>
      </div>
      <div className="account__tabs">
        <SubNav
          items={TABS}
          active={tab}
          onChange={key => setTab(key as AccountTab)}
          ariaLabel="Account sections"
        />
      </div>
      <div className="account__stack">
        {load.state === 'loading' && <Panel><Loading /></Panel>}
        {load.state === 'error' && <SectionError label="Your account" error={load.message} onRetry={reload} />}
        {profile && tab === 'profile' && <ProfileView profile={profile} />}
        {profile && tab === 'company' && <CompanyView canManage={canManage} />}
        {profile && tab === 'subscription' && <SubscriptionView />}
        {profile && tab === 'users' && <UsersView profile={profile} />}
        {profile && tab === 'keys' && <KeysView canManage={canManage} />}
      </div>
    </div>
  )
}

export default Account
