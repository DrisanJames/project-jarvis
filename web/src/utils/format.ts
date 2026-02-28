/**
 * Shared formatting utilities for consistent number, currency, and rate display
 * across all analytics dashboards.
 */

/**
 * Format a number with abbreviated suffixes (K, M, B).
 * @param n - The number to format
 * @param decimals - Decimal places (default: 1)
 */
export const formatNumber = (n: number | undefined | null, decimals = 1): string => {
  if (n == null || isNaN(n)) return '0';
  if (Math.abs(n) >= 1_000_000_000) return (n / 1_000_000_000).toFixed(decimals) + 'B';
  if (Math.abs(n) >= 1_000_000) return (n / 1_000_000).toFixed(decimals) + 'M';
  if (Math.abs(n) >= 1_000) return (n / 1_000).toFixed(decimals) + 'K';
  return n.toFixed(decimals === 0 ? 0 : decimals);
};

/**
 * Format a rate as a percentage string (e.g. 0.5073 -> "50.73%").
 * If the value is already above 1, it's treated as already a percentage.
 * @param rate - Rate as decimal (0-1) or percentage (0-100)
 * @param decimals - Decimal places (default: 2)
 */
export const formatPercent = (rate: number | undefined | null, decimals = 2): string => {
  if (rate == null || isNaN(rate)) return '0%';
  const pct = rate <= 1 && rate >= -1 ? rate * 100 : rate;
  return pct.toFixed(decimals) + '%';
};

/**
 * Format a value as currency.
 * @param n - The number to format
 * @param decimals - Decimal places (default: 2)
 */
export const formatCurrency = (n: number | undefined | null, decimals = 2): string => {
  if (n == null || isNaN(n)) return '$0';
  if (Math.abs(n) >= 1_000_000) return '$' + (n / 1_000_000).toFixed(decimals) + 'M';
  if (Math.abs(n) >= 1_000) return '$' + (n / 1_000).toFixed(decimals) + 'K';
  return '$' + n.toFixed(decimals);
};

/**
 * Format a raw number with commas (e.g. 1234567 -> "1,234,567").
 */
export const formatWithCommas = (n: number | undefined | null): string => {
  if (n == null || isNaN(n)) return '0';
  return n.toLocaleString();
};

/* ─── Deliverability thresholds (configurable) ─── */

export interface RateThresholds {
  delivery: { good: number; warn: number };
  open: { good: number; warn: number };
  click: { good: number; warn: number };
  bounce: { good: number; warn: number };
  complaint: { good: number; warn: number };
}

/** Default industry thresholds – importable and overridable. */
export const DEFAULT_THRESHOLDS: RateThresholds = {
  delivery:  { good: 95, warn: 90 },
  open:      { good: 20, warn: 10 },
  click:     { good: 3,  warn: 1 },
  bounce:    { good: 2,  warn: 5 },    // inverted: lower is better
  complaint: { good: 0.1, warn: 0.3 }, // inverted: lower is better
};

/**
 * Return a CSS class ('good', 'warning', 'bad') based on value and thresholds.
 * @param value - Metric value
 * @param thresholds - { good, warn } thresholds
 * @param invert - If true, lower is better (e.g. bounce/complaint)
 */
export const rateClass = (
  value: number,
  thresholds: { good: number; warn: number },
  invert = false,
): string => {
  if (invert) {
    if (value <= thresholds.good) return 'good';
    if (value <= thresholds.warn) return 'warning';
    return 'bad';
  }
  if (value >= thresholds.good) return 'good';
  if (value >= thresholds.warn) return 'warning';
  return 'bad';
};

/**
 * Return a color hex code based on rate thresholds.
 */
export const rateColor = (
  value: number,
  thresholds: { good: number; warn: number },
  invert = false,
): string => {
  if (invert) {
    if (value <= thresholds.good) return '#10b981';
    if (value <= thresholds.warn) return '#f59e0b';
    return '#ef4444';
  }
  if (value >= thresholds.good) return '#10b981';
  if (value >= thresholds.warn) return '#f59e0b';
  return '#ef4444';
};

/**
 * Grade thresholds for health score calculation.
 */
export const gradeFromScore = (score: number): { grade: string; color: string } => {
  if (score >= 90) return { grade: 'A', color: '#22c55e' };
  if (score >= 80) return { grade: 'B', color: '#3b82f6' };
  if (score >= 70) return { grade: 'C', color: '#f59e0b' };
  if (score >= 60) return { grade: 'D', color: '#ef4444' };
  return { grade: 'F', color: '#ef4444' };
};
