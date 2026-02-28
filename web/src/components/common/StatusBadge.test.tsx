import { render, screen } from '@testing-library/react';
import { describe, it, expect } from 'vitest';
import { StatusBadge } from './StatusBadge';

describe('StatusBadge', () => {
  it('renders healthy status', () => {
    render(<StatusBadge status="healthy" />);
    expect(screen.getByText('Healthy')).toBeInTheDocument();
  });

  it('renders warning status', () => {
    render(<StatusBadge status="warning" />);
    expect(screen.getByText('Warning')).toBeInTheDocument();
  });

  it('renders critical status', () => {
    render(<StatusBadge status="critical" />);
    expect(screen.getByText('Critical')).toBeInTheDocument();
  });

  it('renders custom label when provided', () => {
    render(<StatusBadge status="healthy" label="All Good" />);
    expect(screen.getByText('All Good')).toBeInTheDocument();
    expect(screen.queryByText('Healthy')).not.toBeInTheDocument();
  });

  it('applies correct CSS class for status', () => {
    const { container } = render(<StatusBadge status="critical" />);
    expect(container.querySelector('.status-badge')).toHaveClass('critical');
  });

  it('hides icon when showIcon is false', () => {
    const { container } = render(<StatusBadge status="healthy" showIcon={false} />);
    // Should only have text, no SVG icon
    const badge = container.querySelector('.status-badge');
    expect(badge?.querySelector('svg')).toBeNull();
  });
});
