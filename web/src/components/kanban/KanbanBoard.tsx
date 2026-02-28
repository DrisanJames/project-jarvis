import React, { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faSync, faBolt, faChartBar, faExclamationTriangle } from '@fortawesome/free-solid-svg-icons';
import { Card, CardHeader, CardBody } from '../common/Card';
import { KanbanColumn } from './KanbanColumn';
import { CardModal } from './CardModal';
import { CreateCardModal } from './CreateCardModal';
import { VelocityModal } from './VelocityModal';
import { NotificationToast } from './NotificationToast';
import type { 
  KanbanBoard as KanbanBoardType, 
  Card as CardType, 
  DueTasksResponse,
  AIAnalysisResult,
  CreateCardRequest,
  UpdateCardRequest,
  MoveCardRequest,
} from './types';

interface KanbanBoardProps {
  autoRefresh?: boolean;
  refreshInterval?: number;
}

export const KanbanBoard: React.FC<KanbanBoardProps> = ({
  autoRefresh = true,
  refreshInterval = 30,
}) => {
  const [board, setBoard] = useState<KanbanBoardType | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [isRefreshing, setIsRefreshing] = useState(false);
  
  // Modal states
  const [selectedCard, setSelectedCard] = useState<CardType | null>(null);
  const [showCreateModal, setShowCreateModal] = useState(false);
  const [createColumnId, setCreateColumnId] = useState<string>('backlog');
  const [showVelocityModal, setShowVelocityModal] = useState(false);
  
  // Notification states
  const [notifications, setNotifications] = useState<Array<{
    id: string;
    type: 'due' | 'ai' | 'info';
    title: string;
    message: string;
    cardId?: string;
  }>>([]);
  
  // AI analysis state
  const [isRunningAI, setIsRunningAI] = useState(false);

  // Drag state
  const [draggedCard, setDraggedCard] = useState<CardType | null>(null);
  const [draggedFromColumn, setDraggedFromColumn] = useState<string | null>(null);

  const fetchBoard = useCallback(async (showLoading = true) => {
    try {
      if (showLoading) setLoading(true);
      setIsRefreshing(true);
      
      const response = await fetch('/api/kanban/board');
      if (!response.ok) {
        throw new Error(`HTTP ${response.status}`);
      }
      
      const data = await response.json();
      setBoard(data);
      setError(null);
    } catch (err) {
      console.error('Failed to fetch board:', err);
      setError(err instanceof Error ? err.message : 'Failed to fetch board');
    } finally {
      setLoading(false);
      setIsRefreshing(false);
    }
  }, []);

  const checkDueTasks = useCallback(async () => {
    try {
      const response = await fetch('/api/kanban/due');
      if (!response.ok) return;
      
      const data: DueTasksResponse = await response.json();
      
      // Add notifications for overdue and due today tasks
      const newNotifications: typeof notifications = [];
      
      data.overdue?.forEach(card => {
        if (!notifications.find(n => n.cardId === card.id && n.type === 'due')) {
          newNotifications.push({
            id: `due-${card.id}`,
            type: 'due',
            title: 'Overdue Task',
            message: card.title,
            cardId: card.id,
          });
        }
      });
      
      data.due_today?.forEach(card => {
        if (!notifications.find(n => n.cardId === card.id && n.type === 'due')) {
          newNotifications.push({
            id: `due-${card.id}`,
            type: 'due',
            title: 'Due Today',
            message: card.title,
            cardId: card.id,
          });
        }
      });
      
      if (newNotifications.length > 0) {
        setNotifications(prev => [...prev, ...newNotifications]);
      }
    } catch (err) {
      console.error('Failed to check due tasks:', err);
    }
  }, [notifications]);

  // Initial fetch
  useEffect(() => {
    fetchBoard();
  }, [fetchBoard]);

  // Auto refresh
  useEffect(() => {
    if (!autoRefresh) return;
    
    const boardInterval = setInterval(() => fetchBoard(false), refreshInterval * 1000);
    const dueInterval = setInterval(checkDueTasks, 60000); // Check due tasks every minute
    
    // Initial due check
    checkDueTasks();
    
    return () => {
      clearInterval(boardInterval);
      clearInterval(dueInterval);
    };
  }, [autoRefresh, refreshInterval, fetchBoard, checkDueTasks]);

  const handleDragStart = (card: CardType, columnId: string) => {
    setDraggedCard(card);
    setDraggedFromColumn(columnId);
  };

  const handleDragEnd = () => {
    setDraggedCard(null);
    setDraggedFromColumn(null);
  };

  const handleDrop = async (toColumnId: string, dropIndex: number) => {
    if (!draggedCard || !draggedFromColumn || !board) return;

    const moveRequest: MoveCardRequest = {
      card_id: draggedCard.id,
      from_column: draggedFromColumn,
      to_column: toColumnId,
      new_order: dropIndex,
    };

    // Optimistic update
    const updatedBoard = { ...board };
    const fromColIndex = updatedBoard.columns.findIndex(c => c.id === draggedFromColumn);
    const toColIndex = updatedBoard.columns.findIndex(c => c.id === toColumnId);

    if (fromColIndex !== -1 && toColIndex !== -1) {
      // Remove from source
      const cardIndex = updatedBoard.columns[fromColIndex].cards.findIndex(c => c.id === draggedCard.id);
      if (cardIndex !== -1) {
        const [movedCard] = updatedBoard.columns[fromColIndex].cards.splice(cardIndex, 1);
        
        // Add to destination
        movedCard.order = dropIndex;
        updatedBoard.columns[toColIndex].cards.splice(dropIndex, 0, movedCard);
        
        // Reorder destination column
        updatedBoard.columns[toColIndex].cards.forEach((c, i) => {
          c.order = i;
        });

        setBoard(updatedBoard);
      }
    }

    // Sync with server
    try {
      const response = await fetch(`/api/kanban/cards/${draggedCard.id}/move`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(moveRequest),
      });

      if (!response.ok) {
        // Revert on error
        fetchBoard(false);
      }
    } catch (err) {
      console.error('Failed to move card:', err);
      fetchBoard(false);
    }

    handleDragEnd();
  };

  const handleCreateCard = async (request: CreateCardRequest) => {
    try {
      const response = await fetch('/api/kanban/cards', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(request),
      });

      if (!response.ok) {
        throw new Error('Failed to create card');
      }

      setShowCreateModal(false);
      fetchBoard(false);
    } catch (err) {
      console.error('Failed to create card:', err);
    }
  };

  const handleUpdateCard = async (cardId: string, request: UpdateCardRequest) => {
    try {
      const response = await fetch(`/api/kanban/cards/${cardId}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(request),
      });

      if (!response.ok) {
        throw new Error('Failed to update card');
      }

      setSelectedCard(null);
      fetchBoard(false);
    } catch (err) {
      console.error('Failed to update card:', err);
    }
  };

  const handleDeleteCard = async (cardId: string) => {
    try {
      const response = await fetch(`/api/kanban/cards/${cardId}`, {
        method: 'DELETE',
      });

      if (!response.ok) {
        throw new Error('Failed to delete card');
      }

      setSelectedCard(null);
      fetchBoard(false);
    } catch (err) {
      console.error('Failed to delete card:', err);
    }
  };

  const handleCompleteCard = async (cardId: string) => {
    try {
      const response = await fetch(`/api/kanban/cards/${cardId}/complete`, {
        method: 'POST',
      });

      if (!response.ok) {
        throw new Error('Failed to complete card');
      }

      setSelectedCard(null);
      fetchBoard(false);
    } catch (err) {
      console.error('Failed to complete card:', err);
    }
  };

  const handleTriggerAI = async () => {
    setIsRunningAI(true);
    try {
      const response = await fetch('/api/kanban/ai/trigger', {
        method: 'POST',
      });

      if (!response.ok) {
        throw new Error('Failed to trigger AI analysis');
      }

      const result: AIAnalysisResult = await response.json();

      if (result.new_tasks && result.new_tasks.length > 0) {
        setNotifications(prev => [...prev, {
          id: `ai-${Date.now()}`,
          type: 'ai',
          title: 'AI Tasks Created',
          message: `${result.new_tasks.length} new tasks generated`,
        }]);
        fetchBoard(false);
      } else {
        setNotifications(prev => [...prev, {
          id: `ai-${Date.now()}`,
          type: 'info',
          title: 'AI Analysis Complete',
          message: result.rate_limited ? 'Rate limited, try again later' : 'No new tasks needed',
        }]);
      }
    } catch (err) {
      console.error('Failed to trigger AI:', err);
    } finally {
      setIsRunningAI(false);
    }
  };

  const dismissNotification = (id: string) => {
    setNotifications(prev => prev.filter(n => n.id !== id));
  };

  const openCreateModal = (columnId: string) => {
    setCreateColumnId(columnId);
    setShowCreateModal(true);
  };

  if (loading && !board) {
    return (
      <div className="dashboard-container">
        <div style={{ textAlign: 'center', padding: '4rem', color: 'var(--text-muted)' }}>
          <FontAwesomeIcon icon={faSync} spin style={{ marginBottom: '1rem', fontSize: '32px' }} />
          <div>Loading Kanban Board...</div>
        </div>
      </div>
    );
  }

  if (error && !board) {
    return (
      <div className="dashboard-container">
        <Card>
          <CardBody>
            <div style={{ textAlign: 'center', padding: '2rem' }}>
              <FontAwesomeIcon icon={faExclamationTriangle} style={{ marginBottom: '1rem', fontSize: '48px', color: 'var(--accent-yellow)' }} />
              <h3 style={{ marginBottom: '0.5rem' }}>Unable to Load Kanban Board</h3>
              <p style={{ color: 'var(--text-muted)', marginBottom: '1rem' }}>{error}</p>
              <button 
                onClick={() => fetchBoard()}
                className="btn-primary"
              >
                Retry
              </button>
            </div>
          </CardBody>
        </Card>
      </div>
    );
  }

  return (
    <div className="dashboard-container">
      {/* Header */}
      <Card style={{ marginBottom: '1.5rem' }}>
        <CardHeader
          title={
            <span style={{ display: 'flex', alignItems: 'center', gap: '12px' }}>
              <FontAwesomeIcon icon={faChartBar} />
              Task Management
              <span style={{ 
                fontSize: '0.75rem', 
                padding: '2px 8px', 
                borderRadius: '12px',
                backgroundColor: 'var(--bg-secondary)',
                color: 'var(--text-muted)',
              }}>
                {board?.active_task_count || 0} / {board?.max_active_tasks || 20} active
              </span>
            </span>
          }
          action={
            <div style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
              <button
                onClick={handleTriggerAI}
                disabled={isRunningAI}
                className="btn-secondary"
                style={{ display: 'flex', alignItems: 'center', gap: '6px' }}
                title="Run AI Analysis"
              >
                <FontAwesomeIcon icon={faBolt} className={isRunningAI ? 'animate-pulse' : ''} />
                {isRunningAI ? 'Analyzing...' : 'AI Analyze'}
              </button>
              <button
                onClick={() => setShowVelocityModal(true)}
                className="btn-secondary"
                title="View Velocity Report"
              >
                <FontAwesomeIcon icon={faChartBar} />
              </button>
              <button
                onClick={() => fetchBoard(false)}
                disabled={isRefreshing}
                className="btn-secondary"
              >
                <FontAwesomeIcon icon={faSync} spin={isRefreshing} />
              </button>
            </div>
          }
        />
      </Card>

      {/* Kanban Columns */}
      <div className="kanban-container">
        {board?.columns.map(column => (
          <KanbanColumn
            key={column.id}
            column={column}
            onCardClick={setSelectedCard}
            onAddCard={() => openCreateModal(column.id)}
            onDragStart={handleDragStart}
            onDragEnd={handleDragEnd}
            onDrop={handleDrop}
            isDragging={!!draggedCard}
            draggedCardId={draggedCard?.id}
          />
        ))}
      </div>

      {/* Modals */}
      {selectedCard && (
        <CardModal
          card={selectedCard}
          onClose={() => setSelectedCard(null)}
          onUpdate={handleUpdateCard}
          onDelete={handleDeleteCard}
          onComplete={handleCompleteCard}
        />
      )}

      {showCreateModal && (
        <CreateCardModal
          columnId={createColumnId}
          onClose={() => setShowCreateModal(false)}
          onCreate={handleCreateCard}
        />
      )}

      {showVelocityModal && (
        <VelocityModal onClose={() => setShowVelocityModal(false)} />
      )}

      {/* Notifications */}
      <div className="notification-container">
        {notifications.map(notif => (
          <NotificationToast
            key={notif.id}
            type={notif.type}
            title={notif.title}
            message={notif.message}
            onDismiss={() => dismissNotification(notif.id)}
            onClick={() => {
              if (notif.cardId && board) {
                // Find and select the card
                for (const col of board.columns) {
                  const card = col.cards.find(c => c.id === notif.cardId);
                  if (card) {
                    setSelectedCard(card);
                    break;
                  }
                }
              }
              dismissNotification(notif.id);
            }}
          />
        ))}
      </div>
    </div>
  );
};

export default KanbanBoard;
