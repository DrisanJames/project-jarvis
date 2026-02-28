import React from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faClock, faBolt, faExclamationCircle, faTag } from '@fortawesome/free-solid-svg-icons';
import type { Card, Priority } from './types';

interface KanbanCardProps {
  card: Card;
  onClick: () => void;
  onDragStart: () => void;
  onDragEnd: () => void;
  isDragging: boolean;
}

const priorityColors: Record<Priority, string> = {
  normal: 'var(--text-muted)',
  high: 'var(--accent-yellow)',
  critical: 'var(--accent-red)',
};

const sourceLabels: Record<string, string> = {
  deliverability: 'Deliverability',
  revenue: 'Revenue',
  data_pipeline: 'Data Pipeline',
};

export const KanbanCard: React.FC<KanbanCardProps> = ({
  card,
  onClick,
  onDragStart,
  onDragEnd,
  isDragging,
}) => {
  const isOverdue = card.due_date && new Date(card.due_date) < new Date() && !card.completed_at;
  const isDueToday = card.due_date && !isOverdue && 
    new Date(card.due_date).toDateString() === new Date().toDateString();

  const handleDragStart = (e: React.DragEvent) => {
    e.dataTransfer.effectAllowed = 'move';
    e.dataTransfer.setData('text/plain', card.id);
    onDragStart();
  };

  const formatDueDate = (dateStr: string) => {
    const date = new Date(dateStr);
    const now = new Date();
    const diffDays = Math.ceil((date.getTime() - now.getTime()) / (1000 * 60 * 60 * 24));
    
    if (diffDays < 0) return `${Math.abs(diffDays)}d overdue`;
    if (diffDays === 0) return 'Due today';
    if (diffDays === 1) return 'Due tomorrow';
    if (diffDays < 7) return `Due in ${diffDays}d`;
    return date.toLocaleDateString('en-US', { month: 'short', day: 'numeric' });
  };

  return (
    <div
      className={`kanban-card ${isDragging ? 'dragging' : ''} priority-${card.priority}`}
      draggable
      onDragStart={handleDragStart}
      onDragEnd={onDragEnd}
      onClick={onClick}
    >
      {/* Priority indicator */}
      <div 
        className="kanban-card-priority-bar"
        style={{ backgroundColor: priorityColors[card.priority] }}
      />

      {/* Header with badges */}
      <div className="kanban-card-header">
        {card.ai_generated && (
          <span className="kanban-badge ai" title="AI Generated">
            <FontAwesomeIcon icon={faBolt} />
            AI
          </span>
        )}
        {card.priority === 'critical' && (
          <span className="kanban-badge critical" title="Critical Priority">
            <FontAwesomeIcon icon={faExclamationCircle} />
            Critical
          </span>
        )}
        {card.priority === 'high' && (
          <span className="kanban-badge high" title="High Priority">
            High
          </span>
        )}
      </div>

      {/* Title */}
      <h4 className="kanban-card-title">{card.title}</h4>

      {/* Description preview */}
      {card.description && (
        <p className="kanban-card-description">
          {card.description.length > 80 
            ? card.description.substring(0, 80) + '...' 
            : card.description}
        </p>
      )}

      {/* AI Context */}
      {card.ai_context && (
        <div className="kanban-card-ai-context">
          <span className="kanban-ai-source">
            {sourceLabels[card.ai_context.source] || card.ai_context.source}
          </span>
        </div>
      )}

      {/* Labels */}
      {card.labels && card.labels.length > 0 && (
        <div className="kanban-card-labels">
          {card.labels.slice(0, 3).map((label, i) => (
            <span key={i} className="kanban-label">
              <FontAwesomeIcon icon={faTag} />
              {label}
            </span>
          ))}
          {card.labels.length > 3 && (
            <span className="kanban-label-more">+{card.labels.length - 3}</span>
          )}
        </div>
      )}

      {/* Footer */}
      <div className="kanban-card-footer">
        {card.due_date && (
          <span 
            className={`kanban-due-date ${isOverdue ? 'overdue' : ''} ${isDueToday ? 'today' : ''}`}
            title={new Date(card.due_date).toLocaleString()}
          >
            <FontAwesomeIcon icon={faClock} />
            {formatDueDate(card.due_date)}
          </span>
        )}
        <span className="kanban-created-by">
          {card.created_by === 'ai' ? 'AI' : 'Manual'}
        </span>
      </div>
    </div>
  );
};
