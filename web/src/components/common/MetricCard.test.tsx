import { render, screen } from '@testing-library/react';
import { describe, it, expect } from 'vitest';
import { MetricCard } from './MetricCard';

describe('MetricCard', () => {
  it('renders label and value', () => {
    render(<MetricCard label="Test Label" value={500} />);
    expect(screen.getByText('Test Label')).toBeInTheDocument();
    expect(screen.getByText('500')).toBeInTheDocument();
  });

  it('formats large numbers correctly', () => {
    render(<MetricCard label="Volume" value={1500000} />);
    expect(screen.getByText('1.5M')).toBeInTheDocument();
  });

  it('formats thousands correctly', () => {
    render(<MetricCard label="Count" value={45000} />);
    expect(screen.getByText('45.0K')).toBeInTheDocument();
  });

  it('formats percentage correctly', () => {
    render(<MetricCard label="Rate" value={0.1234} format="percentage" />);
    expect(screen.getByText('12.34%')).toBeInTheDocument();
  });

  it('displays positive change', () => {
    render(<MetricCard label="Test" value={100} change={0.05} />);
    expect(screen.getByText('+5.0%')).toBeInTheDocument();
  });

  it('displays negative change', () => {
    render(<MetricCard label="Test" value={100} change={-0.03} />);
    expect(screen.getByText('-3.0%')).toBeInTheDocument();
  });

  it('displays change label when provided', () => {
    render(
      <MetricCard 
        label="Test" 
        value={100} 
        change={0.1} 
        changeLabel="vs yesterday" 
      />
    );
    expect(screen.getByText('vs yesterday')).toBeInTheDocument();
  });

  it('renders string value correctly', () => {
    render(<MetricCard label="Status" value="Active" />);
    expect(screen.getByText('Active')).toBeInTheDocument();
  });
});
