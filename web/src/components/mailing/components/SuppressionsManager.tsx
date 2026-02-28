import React, { useState, useEffect, useCallback } from 'react';
import './SuppressionsManager.css';

interface Suppression {
  id: string;
  email: string;
  reason: string;
  source: string;
  created_at: string;
}

export const SuppressionsManager: React.FC = () => {
  const [suppressions, setSuppressions] = useState<Suppression[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [showAddForm, setShowAddForm] = useState(false);
  const [newEmail, setNewEmail] = useState('');
  const [newReason, setNewReason] = useState('');

  const fetchSuppressions = useCallback(async () => {
    try {
      const response = await fetch('/api/mailing/suppressions');
      const data = await response.json();
      setSuppressions(data.suppressions || []);
    } catch (err) {
      setError('Failed to load suppressions');
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    fetchSuppressions();
  }, [fetchSuppressions]);

  const handleAddSuppression = async (e: React.FormEvent) => {
    e.preventDefault();
    try {
      const response = await fetch('/api/mailing/suppressions', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email: newEmail, reason: newReason }),
      });
      if (response.ok) {
        setNewEmail('');
        setNewReason('');
        setShowAddForm(false);
        fetchSuppressions();
      }
    } catch (err) {
      setError('Failed to add suppression');
    }
  };

  const handleRemoveSuppression = async (email: string) => {
    if (!confirm(`Remove ${email} from suppression list?`)) return;
    try {
      await fetch(`/api/mailing/suppressions/${encodeURIComponent(email)}`, {
        method: 'DELETE',
      });
      fetchSuppressions();
    } catch (err) {
      setError('Failed to remove suppression');
    }
  };

  if (loading) return <div className="loading">Loading suppressions...</div>;

  return (
    <div className="suppressions-manager">
      <div className="suppressions-header">
        <div>
          <h1>🚫 Suppression Lists</h1>
          <p className="subtitle">Manage emails that should not receive communications</p>
        </div>
        <button className="btn-primary" onClick={() => setShowAddForm(true)}>
          + Add Suppression
        </button>
      </div>

      {error && <div className="error-banner">{error}</div>}

      {showAddForm && (
        <div className="modal-overlay">
          <div className="modal">
            <h2>Add Email to Suppression</h2>
            <form onSubmit={handleAddSuppression}>
              <div className="form-group">
                <label>Email Address</label>
                <input
                  type="email"
                  value={newEmail}
                  onChange={(e) => setNewEmail(e.target.value)}
                  placeholder="email@example.com"
                  required
                />
              </div>
              <div className="form-group">
                <label>Reason</label>
                <select value={newReason} onChange={(e) => setNewReason(e.target.value)} required>
                  <option value="">Select reason...</option>
                  <option value="User requested suppression">User requested</option>
                  <option value="Complaint">Complaint</option>
                  <option value="Hard bounce">Hard bounce</option>
                  <option value="Spam trap">Spam trap</option>
                  <option value="Manual removal">Manual removal</option>
                </select>
              </div>
              <div className="form-actions">
                <button type="button" onClick={() => setShowAddForm(false)}>Cancel</button>
                <button type="submit" className="btn-primary">Add to Suppression</button>
              </div>
            </form>
          </div>
        </div>
      )}

      <div className="stats-row">
        <div className="stat-card">
          <div className="stat-value">{suppressions.length}</div>
          <div className="stat-label">Total Suppressed</div>
        </div>
        <div className="stat-card">
          <div className="stat-value">{suppressions.filter(s => s.source === 'manual').length}</div>
          <div className="stat-label">Manual Adds</div>
        </div>
        <div className="stat-card">
          <div className="stat-value">{suppressions.filter(s => s.reason.includes('bounce')).length}</div>
          <div className="stat-label">Bounces</div>
        </div>
        <div className="stat-card">
          <div className="stat-value">{suppressions.filter(s => s.reason.includes('Complaint')).length}</div>
          <div className="stat-label">Complaints</div>
        </div>
      </div>

      <div className="suppressions-table">
        <table>
          <thead>
            <tr>
              <th>Email</th>
              <th>Reason</th>
              <th>Source</th>
              <th>Added</th>
              <th>Actions</th>
            </tr>
          </thead>
          <tbody>
            {suppressions.length === 0 ? (
              <tr>
                <td colSpan={5} className="empty-state">No suppressions found</td>
              </tr>
            ) : (
              suppressions.map((s) => (
                <tr key={s.id}>
                  <td className="email-cell">{s.email}</td>
                  <td>
                    <span className={`reason-badge ${s.reason.toLowerCase().replace(/\s/g, '-')}`}>
                      {s.reason}
                    </span>
                  </td>
                  <td>{s.source}</td>
                  <td>{new Date(s.created_at).toLocaleDateString()}</td>
                  <td>
                    <button 
                      className="btn-danger btn-sm"
                      onClick={() => handleRemoveSuppression(s.email)}
                    >
                      Remove
                    </button>
                  </td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
};

export default SuppressionsManager;
