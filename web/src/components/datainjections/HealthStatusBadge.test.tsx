import { render, screen } from '@testing-library/react';
import { describe, it, expect } from 'vitest';
import { HealthStatusBadge } from './HealthStatusBadge';

describe('HealthStatusBadge', () => {
  it('renders healthy status with green color', () => {
    render(<HealthStatusBadge status="healthy" />);
    expect(screen.getByText('Healthy')).toBeInTheDocument();
  });

  it('renders warning status with yellow color', () => {
    render(<HealthStatusBadge status="warning" />);
    expect(screen.getByText('Warning')).toBeInTheDocument();
  });

  it('renders critical status with red color', () => {
    render(<HealthStatusBadge status="critical" />);
    expect(screen.getByText('Critical')).toBeInTheDocument();
  });

  it('renders unknown status', () => {
    render(<HealthStatusBadge status="unknown" />);
    expect(screen.getByText('Unknown')).toBeInTheDocument();
  });

  it('renders custom label when provided', () => {
    render(<HealthStatusBadge status="healthy" label="All Systems Go" />);
    expect(screen.getByText('All Systems Go')).toBeInTheDocument();
  });

  it('renders without icon when showIcon is false', () => {
    const { container } = render(<HealthStatusBadge status="healthy" showIcon={false} />);
    // Should only have text, not SVG icon
    const svgs = container.querySelectorAll('svg');
    expect(svgs.length).toBe(0);
  });

  it('renders with different sizes', () => {
    const { rerender } = render(<HealthStatusBadge status="healthy" size="small" />);
    expect(screen.getByText('Healthy')).toBeInTheDocument();
    
    rerender(<HealthStatusBadge status="healthy" size="medium" />);
    expect(screen.getByText('Healthy')).toBeInTheDocument();
    
    rerender(<HealthStatusBadge status="healthy" size="large" />);
    expect(screen.getByText('Healthy')).toBeInTheDocument();
  });
});
