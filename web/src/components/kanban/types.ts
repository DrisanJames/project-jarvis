// Kanban Board Types

export interface KanbanBoard {
  pk: string;
  sk: string;
  last_modified: string;
  columns: Column[];
  last_ai_run: string;
  active_task_count: number;
  max_active_tasks: number;
}

export interface Column {
  id: string;
  title: string;
  order: number;
  cards: Card[];
}

export interface Card {
  id: string;
  title: string;
  description: string;
  priority: Priority;
  due_date?: string;
  created_at: string;
  completed_at?: string;
  created_by: 'ai' | 'human';
  ai_generated: boolean;
  ai_context?: AIContext;
  issue_fingerprint?: string;
  labels: string[];
  order: number;
}

export interface AIContext {
  source: AISource;
  reasoning: string;
  data_points: Record<string, string>;
  severity: string;
  entity_type: string;
  entity_id: string;
  generated_at: string;
}

export type Priority = 'normal' | 'high' | 'critical';
export type AISource = 'deliverability' | 'revenue' | 'data_pipeline' | 'campaign';

export interface DueTasksResponse {
  overdue: Card[];
  due_today: Card[];
  due_soon: Card[];
}

export interface AIAnalysisResult {
  new_tasks: Card[];
  skipped_count: number;
  rate_limited: boolean;
  analyzed_at: string;
  next_run_after: string;
}

export interface VelocityReport {
  month: string;
  total_completed: number;
  total_ai_generated: number;
  total_human_created: number;
  avg_completion_hours: number;
  by_priority: Record<string, VelocityStats>;
  by_source: Record<string, VelocityStats>;
  fastest_category: string;
  slowest_category: string;
  ai_generated_percent: number;
  generated_at: string;
}

export interface VelocityStats {
  count: number;
  avg_completion_hours: number;
  min_hours: number;
  max_hours: number;
}

export interface CreateCardRequest {
  title: string;
  description: string;
  priority: Priority;
  due_date?: string;
  column_id: string;
  labels?: string[];
}

export interface UpdateCardRequest {
  title?: string;
  description?: string;
  priority?: Priority;
  due_date?: string;
  labels?: string[];
}

export interface MoveCardRequest {
  card_id: string;
  from_column: string;
  to_column: string;
  new_order: number;
}

// Column IDs
export const COLUMN_BACKLOG = 'backlog';
export const COLUMN_TODO = 'todo';
export const COLUMN_IN_PROGRESS = 'in-progress';
export const COLUMN_REVIEW = 'review';
export const COLUMN_DONE = 'done';

// Priority colors
export const PRIORITY_COLORS: Record<Priority, string> = {
  normal: 'var(--text-muted)',
  high: 'var(--accent-yellow)',
  critical: 'var(--accent-red)',
};

// AI Source labels
export const AI_SOURCE_LABELS: Record<AISource, string> = {
  deliverability: 'Deliverability',
  revenue: 'Revenue',
  data_pipeline: 'Data Pipeline',
  campaign: 'Campaign',
};
