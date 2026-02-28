import React, { useState, useEffect } from 'react';
import './SendTestEmail.css';

interface ThrottleStatus {
  minute_used: number;
  minute_limit: number;
  hour_used: number;
  hour_limit: number;
}

interface SendingProfile {
  id: string;
  name: string;
  vendor_type: string;
  from_name: string;
  from_email: string;
  status: string;
  is_default: boolean;
}

interface SendResult {
  success: boolean;
  message_id?: string;
  sent_at?: string;
  to?: string;
  suppressed?: boolean;
  throttled?: boolean;
  error?: string;
  reason?: string;
  vendor?: string;
  profile_id?: string;
  from_name?: string;
  from_email?: string;
}

export const SendTestEmail: React.FC = () => {
  const [to, setTo] = useState('');
  const [subject, setSubject] = useState('Jarvis Test Email');
  const [fromName, setFromName] = useState('');
  const [fromEmail, setFromEmail] = useState('');
  const [htmlContent, setHtmlContent] = useState('');
  const [sending, setSending] = useState(false);
  const [result, setResult] = useState<SendResult | null>(null);
  const [throttle, setThrottle] = useState<ThrottleStatus | null>(null);
  const [history, setHistory] = useState<SendResult[]>([]);
  
  // Sending Profiles
  const [profiles, setProfiles] = useState<SendingProfile[]>([]);
  const [selectedProfileId, setSelectedProfileId] = useState<string>('');

  useEffect(() => {
    fetchThrottle();
    fetchProfiles();
    const interval = setInterval(fetchThrottle, 30000);
    return () => clearInterval(interval);
  }, []);

  const fetchThrottle = async () => {
    try {
      const response = await fetch('/api/mailing/throttle/status');
      const data = await response.json();
      setThrottle(data);
    } catch (err) {
      console.error('Failed to fetch throttle status');
    }
  };

  const fetchProfiles = async () => {
    try {
      const response = await fetch('/api/mailing/sending-profiles');
      const data = await response.json();
      const activeProfiles = (data.profiles || []).filter((p: SendingProfile) => p.status === 'active');
      setProfiles(activeProfiles);
      
      // Auto-select default profile
      const defaultProfile = activeProfiles.find((p: SendingProfile) => p.is_default);
      if (defaultProfile) {
        setSelectedProfileId(defaultProfile.id);
        setFromName(defaultProfile.from_name);
        setFromEmail(defaultProfile.from_email);
      }
    } catch (err) {
      console.error('Failed to fetch sending profiles');
    }
  };

  const handleProfileChange = (profileId: string) => {
    setSelectedProfileId(profileId);
    const profile = profiles.find(p => p.id === profileId);
    if (profile) {
      setFromName(profile.from_name);
      setFromEmail(profile.from_email);
    }
  };

  const handleSend = async (e: React.FormEvent) => {
    e.preventDefault();
    setSending(true);
    setResult(null);

    try {
      const response = await fetch('/api/mailing/send-test', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          to,
          subject,
          from_name: fromName,
          from_email: fromEmail,
          html_content: htmlContent || getDefaultTemplate(),
          sending_profile_id: selectedProfileId || undefined,
        }),
      });

      const data = await response.json();
      setResult(data);
      if (data.success) {
        setHistory(prev => [data, ...prev.slice(0, 9)]);
        fetchThrottle();
      }
    } catch (err) {
      setResult({ success: false, error: 'Network error' });
    } finally {
      setSending(false);
    }
  };

  const getDefaultTemplate = () => `
<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
</head>
<body style="font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px; background-color: #f5f5f5;">
  <div style="background: white; border-radius: 16px; padding: 32px; box-shadow: 0 2px 8px rgba(0,0,0,0.08);">
    <div style="text-align: center; margin-bottom: 32px;">
      <h1 style="color: #667eea; margin: 0; font-size: 28px;">📬 JARVIS</h1>
      <p style="color: #6b7280; margin: 8px 0 0;">Mailing Platform Test</p>
    </div>
    
    <h2 style="color: #1f2937; margin-bottom: 16px;">${subject}</h2>
    
    <p style="color: #4b5563; line-height: 1.6;">
      This is a test email sent from the Jarvis Mailing Platform.
    </p>
    
    <div style="background: linear-gradient(135deg, #667eea 0%, #764ba2 100%); color: white; padding: 20px; border-radius: 12px; margin: 24px 0;">
      <h3 style="margin: 0 0 12px 0;">✅ Delivery Confirmed</h3>
      <p style="margin: 0; opacity: 0.9; font-size: 14px;">
        Your email infrastructure is working correctly.
      </p>
    </div>
    
    <div style="background: #f9fafb; border-radius: 8px; padding: 16px; margin-top: 24px;">
      <p style="margin: 0; font-size: 13px; color: #6b7280;">
        <strong>Sent via:</strong> SparkPost API<br>
        <strong>From:</strong> ${fromName} &lt;${fromEmail}&gt;<br>
        <strong>Timestamp:</strong> ${new Date().toISOString()}
      </p>
    </div>
  </div>
  
  <p style="text-align: center; color: #9ca3af; font-size: 12px; margin-top: 24px;">
    Powered by Jarvis Mailing Platform
  </p>
</body>
</html>`;

  return (
    <div className="send-test-email">
      <div className="send-header">
        <div>
          <h1>✉️ Send Test Email</h1>
          <p className="subtitle">Send test emails via SparkPost, AWS SES, Mailgun, or other ESPs</p>
        </div>

        {throttle && (
          <div className="throttle-display">
            <div className="throttle-item">
              <span className="throttle-label">Minute</span>
              <span className="throttle-value">{throttle.minute_used}/{throttle.minute_limit}</span>
            </div>
            <div className="throttle-item">
              <span className="throttle-label">Hour</span>
              <span className="throttle-value">{throttle.hour_used}/{throttle.hour_limit}</span>
            </div>
          </div>
        )}
      </div>

      <div className="send-content">
        <form onSubmit={handleSend} className="send-form">
          {/* Sending Profile Selector */}
          {profiles.length > 0 && (
            <div className="form-group full-width profile-selector">
              <label>🚀 Sending Profile (ESP)</label>
              <select 
                value={selectedProfileId} 
                onChange={(e) => handleProfileChange(e.target.value)}
                className="profile-select"
              >
                <option value="">-- Use Default Profile --</option>
                {profiles.map(profile => (
                  <option key={profile.id} value={profile.id}>
                    {profile.name} ({profile.vendor_type.toUpperCase()})
                    {profile.is_default ? ' ⭐ DEFAULT' : ''}
                  </option>
                ))}
              </select>
              {selectedProfileId && (
                <div className="profile-info">
                  Routing via: <strong>{profiles.find(p => p.id === selectedProfileId)?.vendor_type.toUpperCase()}</strong>
                </div>
              )}
            </div>
          )}

          <div className="form-row">
            <div className="form-group">
              <label>To Email *</label>
              <input
                type="email"
                value={to}
                onChange={(e) => setTo(e.target.value)}
                placeholder="recipient@example.com"
                required
              />
            </div>
            <div className="form-group">
              <label>Subject *</label>
              <input
                type="text"
                value={subject}
                onChange={(e) => setSubject(e.target.value)}
                placeholder="Email subject..."
                required
              />
            </div>
          </div>

          <div className="form-row">
            <div className="form-group">
              <label>From Name</label>
              <input
                type="text"
                value={fromName}
                onChange={(e) => setFromName(e.target.value)}
                placeholder="Sender name"
              />
            </div>
            <div className="form-group">
              <label>From Email</label>
              <input
                type="email"
                value={fromEmail}
                onChange={(e) => setFromEmail(e.target.value)}
                placeholder="sender@yourdomain.com"
              />
            </div>
          </div>

          <div className="form-group full-width">
            <label>HTML Content (optional - uses template if empty)</label>
            <textarea
              value={htmlContent}
              onChange={(e) => setHtmlContent(e.target.value)}
              placeholder="<html>...</html>"
              rows={6}
            />
          </div>

          <button type="submit" className="send-button" disabled={sending || !to}>
            {sending ? '📤 Sending...' : '📤 Send Test Email'}
          </button>
        </form>

        {result && (
          <div className={`result-card ${result.success ? 'success' : 'error'}`}>
            {result.success ? (
              <>
                <div className="result-icon">✅</div>
                <div className="result-content">
                  <h3>Email Sent Successfully!</h3>
                  <p>Message ID: <code>{result.message_id}</code></p>
                  <p>To: {result.to}</p>
                  {result.vendor && <p>Via: <strong>{result.vendor.toUpperCase()}</strong></p>}
                  {result.from_email && <p>From: {result.from_name} &lt;{result.from_email}&gt;</p>}
                  <p>Sent at: {result.sent_at}</p>
                </div>
              </>
            ) : result.suppressed ? (
              <>
                <div className="result-icon">🚫</div>
                <div className="result-content">
                  <h3>Email Suppressed</h3>
                  <p>{result.reason}</p>
                </div>
              </>
            ) : result.throttled ? (
              <>
                <div className="result-icon">⏳</div>
                <div className="result-content">
                  <h3>Rate Limited</h3>
                  <p>{result.reason}</p>
                </div>
              </>
            ) : (
              <>
                <div className="result-icon">❌</div>
                <div className="result-content">
                  <h3>Send Failed</h3>
                  <p>{result.error || 'Unknown error'}</p>
                </div>
              </>
            )}
          </div>
        )}

        {history.length > 0 && (
          <div className="send-history">
            <h3>Recent Sends</h3>
            <div className="history-list">
              {history.map((item, idx) => (
                <div key={idx} className="history-item">
                  <span className="history-status">✅</span>
                  <span className="history-to">{item.to}</span>
                  <span className="history-time">
                    {item.sent_at ? new Date(item.sent_at).toLocaleTimeString() : '-'}
                  </span>
                </div>
              ))}
            </div>
          </div>
        )}
      </div>
    </div>
  );
};

export default SendTestEmail;
