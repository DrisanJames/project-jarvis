import { render, screen } from '@testing-library/react';
import { describe, it, expect } from 'vitest';
import { IngestionPanel } from './IngestionPanel';
import type { IngestionSummary } from './types';

const mockIngestionData: IngestionSummary = {
  status: 'healthy',
  total_records: 100000,
  today_records: 5000,
  data_sets_active: 3,
  data_sets_with_gaps: 0,
  data_sets: [
    {
      data_set_code: 'GLB_BR',
      data_partner: 'GLOBE USA',
      data_set_name: 'GLOBE_USA_BEDROCK',
      record_count: 50000,
      today_count: 2500,
      last_timestamp: new Date().toISOString(),
      has_gap: false,
      gap_hours: 0.5,
    },
    {
      data_set_code: 'USA_PR',
      data_partner: 'USA Partners',
      data_set_name: 'USA_PRIME',
      record_count: 50000,
      today_count: 2500,
      last_timestamp: new Date().toISOString(),
      has_gap: false,
      gap_hours: 1,
    },
  ],
  daily_counts: [],
  last_fetch: new Date().toISOString(),
};

describe('IngestionPanel', () => {
  it('renders loading state', () => {
    render(<IngestionPanel data={null} isLoading={true} />);
    expect(screen.getByText(/Loading ingestion data/i)).toBeInTheDocument();
  });

  it('renders not configured state when data is null', () => {
    render(<IngestionPanel data={null} isLoading={false} />);
    expect(screen.getByText(/Azure Table Storage not configured/i)).toBeInTheDocument();
  });

  it('renders not configured state when status is unknown', () => {
    render(<IngestionPanel data={{ ...mockIngestionData, status: 'unknown' }} isLoading={false} />);
    expect(screen.getByText(/Azure Table Storage not configured/i)).toBeInTheDocument();
  });

  it('renders ingestion summary metrics', () => {
    render(<IngestionPanel data={mockIngestionData} isLoading={false} />);
    
    // Check that key metrics are displayed
    expect(screen.getByText(/Total Records/i)).toBeInTheDocument();
    expect(screen.getByText(/Today's Records/i)).toBeInTheDocument();
    expect(screen.getByText(/Active Data Sets/i)).toBeInTheDocument();
  });

  it('renders data sets table', () => {
    render(<IngestionPanel data={mockIngestionData} isLoading={false} />);
    
    // Check that data sets are shown
    expect(screen.getByText('GLB_BR')).toBeInTheDocument();
    expect(screen.getByText('USA_PR')).toBeInTheDocument();
  });

  it('shows health status badge', () => {
    render(<IngestionPanel data={mockIngestionData} isLoading={false} />);
    expect(screen.getByText('Healthy')).toBeInTheDocument();
  });

  it('displays gap warning when data sets have gaps', () => {
    const dataWithGaps: IngestionSummary = {
      ...mockIngestionData,
      status: 'warning',
      data_sets_with_gaps: 1,
      data_sets: [
        {
          ...mockIngestionData.data_sets[0],
          has_gap: true,
          gap_hours: 48,
        },
      ],
    };

    render(<IngestionPanel data={dataWithGaps} isLoading={false} />);
    expect(screen.getByText(/Data Gap Detected/i)).toBeInTheDocument();
  });
});
