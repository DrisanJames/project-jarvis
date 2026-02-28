
type PerformanceLevel = 'optimal' | 'good' | 'average' | 'poor' | 'high' | 'medium' | 'low';

interface PerformanceBadgeProps {
  level: PerformanceLevel;
  showLabel?: boolean;
  size?: 'sm' | 'md' | 'lg';
}

const colors: Record<PerformanceLevel, { bg: string; text: string }> = {
  optimal: { bg: '#c6f6d5', text: '#22543d' },
  good: { bg: '#c6f6d5', text: '#22543d' },
  high: { bg: '#c6f6d5', text: '#22543d' },
  average: { bg: '#fefcbf', text: '#744210' },
  medium: { bg: '#fefcbf', text: '#744210' },
  poor: { bg: '#fed7d7', text: '#822727' },
  low: { bg: '#fed7d7', text: '#822727' },
};

const labels: Record<PerformanceLevel, string> = {
  optimal: 'Optimal',
  good: 'Good',
  high: 'High',
  average: 'Average',
  medium: 'Medium',
  poor: 'Poor',
  low: 'Low',
};

const sizes = {
  sm: { padding: '0.125rem 0.375rem', fontSize: '0.625rem' },
  md: { padding: '0.25rem 0.5rem', fontSize: '0.75rem' },
  lg: { padding: '0.375rem 0.75rem', fontSize: '0.875rem' },
};

export function PerformanceBadge({
  level,
  showLabel = true,
  size = 'md',
}: PerformanceBadgeProps) {
  const color = colors[level] || colors.average;
  const sizeStyles = sizes[size];

  return (
    <span
      style={{
        display: 'inline-flex',
        alignItems: 'center',
        padding: sizeStyles.padding,
        borderRadius: '9999px',
        backgroundColor: color.bg,
        color: color.text,
        fontSize: sizeStyles.fontSize,
        fontWeight: 600,
        textTransform: 'capitalize',
      }}
    >
      {showLabel ? labels[level] || level : '●'}
    </span>
  );
}

export default PerformanceBadge;
