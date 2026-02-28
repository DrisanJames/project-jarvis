import React, { useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faPlus } from '@fortawesome/free-solid-svg-icons';
import { KanbanCard } from './KanbanCard';
import type { Column, Card } from './types';

interface KanbanColumnProps {
  column: Column;
  onCardClick: (card: Card) => void;
  onAddCard: () => void;
  onDragStart: (card: Card, columnId: string) => void;
  onDragEnd: () => void;
  onDrop: (columnId: string, index: number) => void;
  isDragging: boolean;
  draggedCardId?: string;
}

export const KanbanColumn: React.FC<KanbanColumnProps> = ({
  column,
  onCardClick,
  onAddCard,
  onDragStart,
  onDragEnd,
  onDrop,
  isDragging,
  draggedCardId,
}) => {
  const [isOver, setIsOver] = useState(false);
  const [dropIndex, setDropIndex] = useState<number | null>(null);

  const handleDragOver = (e: React.DragEvent, index: number) => {
    e.preventDefault();
    setIsOver(true);
    setDropIndex(index);
  };

  const handleDragLeave = () => {
    setIsOver(false);
    setDropIndex(null);
  };

  const handleDrop = (e: React.DragEvent) => {
    e.preventDefault();
    const index = dropIndex ?? column.cards.length;
    onDrop(column.id, index);
    setIsOver(false);
    setDropIndex(null);
  };

  // Count cards by status for the column
  const criticalCount = column.cards.filter(c => c.priority === 'critical').length;
  const highCount = column.cards.filter(c => c.priority === 'high').length;

  return (
    <div 
      className={`kanban-column ${isOver ? 'drag-over' : ''}`}
      onDragOver={(e) => handleDragOver(e, column.cards.length)}
      onDragLeave={handleDragLeave}
      onDrop={handleDrop}
    >
      {/* Column Header */}
      <div className="kanban-column-header">
        <div className="kanban-column-title">
          <span>{column.title}</span>
          <span className="kanban-column-count">{column.cards.length}</span>
          {criticalCount > 0 && (
            <span className="kanban-priority-badge critical">{criticalCount}</span>
          )}
          {highCount > 0 && (
            <span className="kanban-priority-badge high">{highCount}</span>
          )}
        </div>
        <button 
          className="kanban-add-btn"
          onClick={onAddCard}
          title="Add card"
        >
          <FontAwesomeIcon icon={faPlus} />
        </button>
      </div>

      {/* Cards */}
      <div className="kanban-column-cards">
        {column.cards.map((card, index) => (
          <React.Fragment key={card.id}>
            {/* Drop zone before card */}
            {isDragging && dropIndex === index && draggedCardId !== card.id && (
              <div className="kanban-drop-indicator" />
            )}
            <div
              onDragOver={(e) => handleDragOver(e, index)}
            >
              <KanbanCard
                card={card}
                onClick={() => onCardClick(card)}
                onDragStart={() => onDragStart(card, column.id)}
                onDragEnd={onDragEnd}
                isDragging={draggedCardId === card.id}
              />
            </div>
          </React.Fragment>
        ))}
        
        {/* Drop zone at end */}
        {isDragging && (dropIndex === column.cards.length || (isOver && dropIndex === null)) && (
          <div className="kanban-drop-indicator" />
        )}
        
        {/* Empty state */}
        {column.cards.length === 0 && !isDragging && (
          <div className="kanban-empty-column">
            <p>No tasks</p>
            <button onClick={onAddCard} className="btn-secondary btn-small">
              <FontAwesomeIcon icon={faPlus} /> Add task
            </button>
          </div>
        )}
      </div>
    </div>
  );
};
