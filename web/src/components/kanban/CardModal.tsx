import React, { useState, useEffect } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faTimes, faSave, faTrash, faCheckCircle, faBolt, faClock, faTag } from '@fortawesome/free-solid-svg-icons';
import type { Card, UpdateCardRequest, Priority } from './types';

interface CardModalProps {
  card: Card;
  onClose: () => void;
  onUpdate: (cardId: string, request: UpdateCardRequest) => void;
  onDelete: (cardId: string) => void;
  onComplete: (cardId: string) => void;
}

const priorityOptions: { value: Priority; label: string; color: string }[] = [
  { value: 'normal', label: 'Normal', color: 'var(--text-muted)' },
  { value: 'high', label: 'High', color: 'var(--accent-yellow)' },
  { value: 'critical', label: 'Critical', color: 'var(--accent-red)' },
];

const sourceLabels: Record<string, string> = {
  deliverability: 'Deliverability',
  revenue: 'Revenue',
  data_pipeline: 'Data Pipeline',
};

export const CardModal: React.FC<CardModalProps> = ({
  card,
  onClose,
  onUpdate,
  onDelete,
  onComplete,
}) => {
  const [title, setTitle] = useState(card.title);
  const [description, setDescription] = useState(card.description);
  const [priority, setPriority] = useState<Priority>(card.priority);
  const [dueDate, setDueDate] = useState(card.due_date ? card.due_date.split('T')[0] : '');
  const [hasChanges, setHasChanges] = useState(false);
  const [showDeleteConfirm, setShowDeleteConfirm] = useState(false);

  useEffect(() => {
    const changed = 
      title !== card.title ||
      description !== card.description ||
      priority !== card.priority ||
      dueDate !== (card.due_date ? card.due_date.split('T')[0] : '');
    setHasChanges(changed);
  }, [title, description, priority, dueDate, card]);

  const handleSave = () => {
    const request: UpdateCardRequest = {};
    if (title !== card.title) request.title = title;
    if (description !== card.description) request.description = description;
    if (priority !== card.priority) request.priority = priority;
    if (dueDate !== (card.due_date ? card.due_date.split('T')[0] : '')) {
      request.due_date = dueDate ? new Date(dueDate).toISOString() : undefined;
    }
    onUpdate(card.id, request);
  };

  const handleDelete = () => {
    if (showDeleteConfirm) {
      onDelete(card.id);
    } else {
      setShowDeleteConfirm(true);
    }
  };

  const formatDate = (dateStr: string) => {
    return new Date(dateStr).toLocaleString('en-US', {
      month: 'short',
      day: 'numeric',
      year: 'numeric',
      hour: '2-digit',
      minute: '2-digit',
    });
  };

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal-content kanban-modal" onClick={e => e.stopPropagation()}>
        {/* Header */}
        <div className="modal-header">
          <div className="modal-header-badges">
            {card.ai_generated && (
              <span className="kanban-badge ai">
                <FontAwesomeIcon icon={faBolt} /> AI Generated
              </span>
            )}
            <span 
              className={`kanban-badge ${card.priority}`}
              style={{ backgroundColor: priorityOptions.find(p => p.value === card.priority)?.color }}
            >
              {card.priority.charAt(0).toUpperCase() + card.priority.slice(1)}
            </span>
          </div>
          <button className="modal-close" onClick={onClose}>
            <FontAwesomeIcon icon={faTimes} />
          </button>
        </div>

        {/* Body */}
        <div className="modal-body">
          {/* Title */}
          <div className="form-group">
            <label>Title</label>
            <input
              type="text"
              value={title}
              onChange={e => setTitle(e.target.value)}
              className="form-input"
              placeholder="Task title"
            />
          </div>

          {/* Description */}
          <div className="form-group">
            <label>Description</label>
            <textarea
              value={description}
              onChange={e => setDescription(e.target.value)}
              className="form-textarea"
              placeholder="Task description"
              rows={4}
            />
          </div>

          {/* Priority and Due Date */}
          <div className="form-row">
            <div className="form-group">
              <label>Priority</label>
              <select
                value={priority}
                onChange={e => setPriority(e.target.value as Priority)}
                className="form-select"
              >
                {priorityOptions.map(opt => (
                  <option key={opt.value} value={opt.value}>{opt.label}</option>
                ))}
              </select>
            </div>
            <div className="form-group">
              <label>Due Date</label>
              <input
                type="date"
                value={dueDate}
                onChange={e => setDueDate(e.target.value)}
                className="form-input"
              />
            </div>
          </div>

          {/* AI Context */}
          {card.ai_context && (
            <div className="kanban-ai-details">
              <h4><FontAwesomeIcon icon={faBolt} /> AI Analysis Context</h4>
              <div className="ai-context-grid">
                <div className="ai-context-item">
                  <span className="ai-context-label">Source</span>
                  <span className="ai-context-value">
                    {sourceLabels[card.ai_context.source] || card.ai_context.source}
                  </span>
                </div>
                <div className="ai-context-item">
                  <span className="ai-context-label">Entity</span>
                  <span className="ai-context-value">
                    {card.ai_context.entity_type}: {card.ai_context.entity_id}
                  </span>
                </div>
              </div>
              <div className="ai-reasoning">
                <span className="ai-context-label">Reasoning</span>
                <p>{card.ai_context.reasoning}</p>
              </div>
              {card.ai_context.data_points && Object.keys(card.ai_context.data_points).length > 0 && (
                <div className="ai-data-points">
                  <span className="ai-context-label">Data Points</span>
                  <div className="data-points-grid">
                    {Object.entries(card.ai_context.data_points).map(([key, value]) => (
                      <div key={key} className="data-point">
                        <span className="data-point-key">{key.replace(/_/g, ' ')}</span>
                        <span className="data-point-value">{value}</span>
                      </div>
                    ))}
                  </div>
                </div>
              )}
            </div>
          )}

          {/* Labels */}
          {card.labels && card.labels.length > 0 && (
            <div className="form-group">
              <label>Labels</label>
              <div className="kanban-card-labels">
                {card.labels.map((label, i) => (
                  <span key={i} className="kanban-label">
                    <FontAwesomeIcon icon={faTag} />
                    {label}
                  </span>
                ))}
              </div>
            </div>
          )}

          {/* Metadata */}
          <div className="kanban-metadata">
            <div className="metadata-item">
              <FontAwesomeIcon icon={faClock} />
              Created: {formatDate(card.created_at)}
            </div>
            {card.completed_at && (
              <div className="metadata-item">
                <FontAwesomeIcon icon={faCheckCircle} />
                Completed: {formatDate(card.completed_at)}
              </div>
            )}
          </div>
        </div>

        {/* Footer */}
        <div className="modal-footer">
          <div className="modal-footer-left">
            {showDeleteConfirm ? (
              <>
                <span style={{ color: 'var(--accent-red)', marginRight: '8px' }}>
                  Are you sure?
                </span>
                <button 
                  className="btn-danger"
                  onClick={handleDelete}
                >
                  Yes, Delete
                </button>
                <button 
                  className="btn-secondary"
                  onClick={() => setShowDeleteConfirm(false)}
                >
                  Cancel
                </button>
              </>
            ) : (
              <button 
                className="btn-danger-outline"
                onClick={handleDelete}
              >
                <FontAwesomeIcon icon={faTrash} /> Delete
              </button>
            )}
          </div>
          <div className="modal-footer-right">
            {!card.completed_at && (
              <button 
                className="btn-success"
                onClick={() => onComplete(card.id)}
              >
                <FontAwesomeIcon icon={faCheckCircle} /> Complete
              </button>
            )}
            <button 
              className="btn-primary"
              onClick={handleSave}
              disabled={!hasChanges}
            >
              <FontAwesomeIcon icon={faSave} /> Save Changes
            </button>
          </div>
        </div>
      </div>
    </div>
  );
};
