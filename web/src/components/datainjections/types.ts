// Data Injections API Types

export type HealthStatus = 'healthy' | 'warning' | 'critical' | 'unknown';

// Azure Table Storage Types
export interface DataSetMetrics {
  data_set_code: string;
  data_partner: string;
  data_set_name: string;
  record_count: number;
  today_count: number;
  last_timestamp: string;
  has_gap: boolean;
  gap_hours: number;
}

export interface DailyDataSetCount {
  date: string;
  data_set_code: string;
  count: number;
}

// System Health - Processor status
export interface SystemHealth {
  status: HealthStatus;
  last_hydration_time: string;
  hours_since_hydration: number;
  processor_running: boolean;
}

// Partner Health - Individual partner feed status
export interface PartnerHealth {
  data_partner: string;
  data_set_code: string;
  last_timestamp: string;
  gap_hours: number;
  status: HealthStatus;
}

// Historical metrics for date range comparison
export interface HistoricalMetrics {
  date_range: string;
  start_date: string;
  end_date: string;
  total_records: number;
  daily_average: number;
  daily_counts: DailyDataSetCount[];
}

export interface IngestionSummary {
  status: HealthStatus;
  total_records: number;
  today_records: number;
  accepted_today: number;
  data_sets_active: number;
  data_sets_with_gaps: number;
  data_sets: DataSetMetrics[];
  daily_counts: DailyDataSetCount[];
  last_fetch: string;
  
  // System Health - Processor status (Critical if not running)
  system_health: SystemHealth | null;
  
  // Partner Alerts - Individual partner feed issues (less prominent)
  partner_alerts: PartnerHealth[];
  
  // Historical data for comparison
  historical: Record<string, HistoricalMetrics>;
}

// Snowflake Validation Types
export interface ValidationStatus {
  status_id: string;
  count: number;
}

export interface DailyValidationMetrics {
  date: string;
  total_records: number;
  status_breakdown: ValidationStatus[];
}

export interface DomainGroupMetrics {
  domain_group: string;
  domain_group_short: string;
  count: number;
}

export interface ValidationSummary {
  status: HealthStatus;
  total_records: number;
  today_records: number;
  unique_statuses: number;
  status_breakdown: ValidationStatus[];
  daily_metrics: DailyValidationMetrics[];
  domain_breakdown: DomainGroupMetrics[];
  last_fetch: string;
}

// Ongage Import Types
export interface Import {
  id: string;
  name: string;
  action: string;
  total: string;
  success: string;
  failed: string;
  duplicate: string;
  existing: string;
  progress: string;
  status: string;
  status_desc: string;
  created: string;
  modified: string;
}

export interface DailyImportMetrics {
  date: string;
  total_imports: number;
  total_records: number;
  success_records: number;
  failed_records: number;
  duplicate_records: number;
}

export interface ImportSummary {
  status: HealthStatus;
  total_imports: number;
  today_imports: number;
  total_records: number;
  success_records: number;
  failed_records: number;
  duplicate_records: number;
  in_progress: number;
  completed: number;
  recent_imports: Import[];
  daily_metrics: DailyImportMetrics[];
  last_fetch: string;
}

// Dashboard Types
export interface DataInjectionsDashboard {
  timestamp: string;
  overall_health: HealthStatus;
  health_issues: string[];
  ingestion: IngestionSummary | null;
  validation: ValidationSummary | null;
  import: ImportSummary | null;
}

// API Response Types
export interface HealthResponse {
  timestamp: string;
  status: HealthStatus;
  issues: string[];
}
