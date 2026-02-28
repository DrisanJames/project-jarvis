import React, { useEffect, useState } from 'react';
import { useLists, useSubscribers } from '../hooks/useMailingApi';
import type { List, Subscriber } from '../types';
import './ListsManager.css';

interface CreateListModalProps {
  isOpen: boolean;
  onClose: () => void;
  onSubmit: (list: Partial<List>) => Promise<void>;
}

const CreateListModal: React.FC<CreateListModalProps> = ({ isOpen, onClose, onSubmit }) => {
  const [formData, setFormData] = useState({
    name: '',
    description: '',
    default_from_name: '',
    default_from_email: '',
    default_reply_to: '',
    opt_in_type: 'single',
  });
  const [loading, setLoading] = useState(false);

  if (!isOpen) return null;

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setLoading(true);
    try {
      await onSubmit(formData);
      onClose();
      setFormData({
        name: '',
        description: '',
        default_from_name: '',
        default_from_email: '',
        default_reply_to: '',
        opt_in_type: 'single',
      });
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal-content" onClick={(e) => e.stopPropagation()}>
        <h2>Create New List</h2>
        <form onSubmit={handleSubmit}>
          <div className="form-group">
            <label>List Name *</label>
            <input
              type="text"
              value={formData.name}
              onChange={(e) => setFormData({ ...formData, name: e.target.value })}
              required
              placeholder="e.g., Newsletter Subscribers"
            />
          </div>
          <div className="form-group">
            <label>Description</label>
            <textarea
              value={formData.description}
              onChange={(e) => setFormData({ ...formData, description: e.target.value })}
              placeholder="Brief description of this list"
            />
          </div>
          <div className="form-row">
            <div className="form-group">
              <label>Default From Name</label>
              <input
                type="text"
                value={formData.default_from_name}
                onChange={(e) => setFormData({ ...formData, default_from_name: e.target.value })}
                placeholder="Sender Name"
              />
            </div>
            <div className="form-group">
              <label>Default From Email</label>
              <input
                type="email"
                value={formData.default_from_email}
                onChange={(e) => setFormData({ ...formData, default_from_email: e.target.value })}
                placeholder="sender@example.com"
              />
            </div>
          </div>
          <div className="form-group">
            <label>Reply-To Email</label>
            <input
              type="email"
              value={formData.default_reply_to}
              onChange={(e) => setFormData({ ...formData, default_reply_to: e.target.value })}
              placeholder="reply@example.com"
            />
          </div>
          <div className="form-group">
            <label>Opt-in Type</label>
            <select
              value={formData.opt_in_type}
              onChange={(e) => setFormData({ ...formData, opt_in_type: e.target.value })}
            >
              <option value="single">Single Opt-in</option>
              <option value="double">Double Opt-in</option>
            </select>
          </div>
          <div className="modal-actions">
            <button type="button" className="cancel-btn" onClick={onClose}>
              Cancel
            </button>
            <button type="submit" className="submit-btn" disabled={loading}>
              {loading ? 'Creating...' : 'Create List'}
            </button>
          </div>
        </form>
      </div>
    </div>
  );
};

interface SubscribersViewProps {
  list: List;
  onBack: () => void;
}

const SubscribersView: React.FC<SubscribersViewProps> = ({ list, onBack }) => {
  const { subscribers, total, loading, fetchSubscribers, addSubscriber } = useSubscribers(list.id);
  const [page, setPage] = useState(0);
  const [showAddModal, setShowAddModal] = useState(false);
  const [newEmail, setNewEmail] = useState('');
  const limit = 50;

  useEffect(() => {
    fetchSubscribers(limit, page * limit);
  }, [fetchSubscribers, page]);

  const handleAddSubscriber = async (e: React.FormEvent) => {
    e.preventDefault();
    try {
      await addSubscriber({ email: newEmail });
      setNewEmail('');
      setShowAddModal(false);
    } catch (error) {
      alert('Failed to add subscriber');
    }
  };

  const getStatusColor = (status: string) => {
    const colors: Record<string, string> = {
      confirmed: '#22c55e',
      unconfirmed: '#f59e0b',
      unsubscribed: '#6b7280',
      bounced: '#ef4444',
      complained: '#dc2626',
    };
    return colors[status] || '#6b7280';
  };

  return (
    <div className="subscribers-view">
      <div className="subscribers-header">
        <button className="back-btn" onClick={onBack}>
          ← Back to Lists
        </button>
        <div className="list-info">
          <h2>{list.name}</h2>
          <p>{total.toLocaleString()} subscribers</p>
        </div>
        <button className="add-btn" onClick={() => setShowAddModal(true)}>
          + Add Subscriber
        </button>
      </div>

      {loading && <div className="loading">Loading subscribers...</div>}

      <table className="subscribers-table">
        <thead>
          <tr>
            <th>Email</th>
            <th>Name</th>
            <th>Status</th>
            <th>Engagement</th>
            <th>Opens</th>
            <th>Clicks</th>
            <th>Subscribed</th>
          </tr>
        </thead>
        <tbody>
          {subscribers.map((sub) => (
            <tr key={sub.id}>
              <td className="email-cell">{sub.email}</td>
              <td>{[sub.first_name, sub.last_name].filter(Boolean).join(' ') || '-'}</td>
              <td>
                <span
                  className="status-dot"
                  style={{ backgroundColor: getStatusColor(sub.status) }}
                />
                {sub.status}
              </td>
              <td>
                <div className="engagement-bar">
                  <div
                    className="engagement-fill"
                    style={{ width: `${sub.engagement_score}%` }}
                  />
                </div>
                <span className="engagement-score">{sub.engagement_score.toFixed(0)}</span>
              </td>
              <td>{sub.total_opens}</td>
              <td>{sub.total_clicks}</td>
              <td>{new Date(sub.subscribed_at).toLocaleDateString()}</td>
            </tr>
          ))}
        </tbody>
      </table>

      <div className="pagination">
        <button disabled={page === 0} onClick={() => setPage(page - 1)}>
          Previous
        </button>
        <span>
          Page {page + 1} of {Math.ceil(total / limit)}
        </span>
        <button
          disabled={(page + 1) * limit >= total}
          onClick={() => setPage(page + 1)}
        >
          Next
        </button>
      </div>

      {showAddModal && (
        <div className="modal-overlay" onClick={() => setShowAddModal(false)}>
          <div className="modal-content small" onClick={(e) => e.stopPropagation()}>
            <h3>Add Subscriber</h3>
            <form onSubmit={handleAddSubscriber}>
              <div className="form-group">
                <label>Email Address</label>
                <input
                  type="email"
                  value={newEmail}
                  onChange={(e) => setNewEmail(e.target.value)}
                  required
                  placeholder="subscriber@example.com"
                />
              </div>
              <div className="modal-actions">
                <button type="button" onClick={() => setShowAddModal(false)}>
                  Cancel
                </button>
                <button type="submit" className="submit-btn">
                  Add
                </button>
              </div>
            </form>
          </div>
        </div>
      )}
    </div>
  );
};

export const ListsManager: React.FC = () => {
  const { lists, loading, error, fetchLists, createList } = useLists();
  const [showCreateModal, setShowCreateModal] = useState(false);
  const [selectedList, setSelectedList] = useState<List | null>(null);

  useEffect(() => {
    fetchLists();
  }, [fetchLists]);

  const handleCreateList = async (list: Partial<List>) => {
    await createList(list);
  };

  if (selectedList) {
    return (
      <SubscribersView
        list={selectedList}
        onBack={() => setSelectedList(null)}
      />
    );
  }

  return (
    <div className="lists-manager">
      <header className="lists-header">
        <div>
          <h1>Mailing Lists</h1>
          <p>Manage your subscriber lists</p>
        </div>
        <button className="create-btn" onClick={() => setShowCreateModal(true)}>
          + Create List
        </button>
      </header>

      {error && <div className="error-message">{error}</div>}

      {loading && lists.length === 0 && (
        <div className="loading-state">Loading lists...</div>
      )}

      <div className="lists-grid">
        {lists.map((list) => (
          <div
            key={list.id}
            className="list-card"
            onClick={() => setSelectedList(list)}
          >
            <h3>{list.name}</h3>
            <p className="list-description">{list.description || 'No description'}</p>
            <div className="list-stats">
              <div className="stat">
                <span className="stat-value">{list.subscriber_count.toLocaleString()}</span>
                <span className="stat-label">Total</span>
              </div>
              <div className="stat">
                <span className="stat-value">{list.active_count.toLocaleString()}</span>
                <span className="stat-label">Active</span>
              </div>
            </div>
            <div className="list-meta">
              <span className={`status-badge status-${list.status}`}>{list.status}</span>
              <span className="created-date">
                Created {new Date(list.created_at).toLocaleDateString()}
              </span>
            </div>
          </div>
        ))}
      </div>

      <CreateListModal
        isOpen={showCreateModal}
        onClose={() => setShowCreateModal(false)}
        onSubmit={handleCreateList}
      />
    </div>
  );
};

export default ListsManager;
