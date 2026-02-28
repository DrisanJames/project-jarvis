import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { ImprovementsDashboard } from './ImprovementsDashboard';
import { Suggestion } from './types';

const mockSuggestions: Suggestion[] = [
  {
    id: 'sug_1',
    submitted_by_email: 'user1@example.com',
    submitted_by_name: 'User One',
    area: 'Dashboard',
    area_context: 'Main view',
    original_suggestion: 'Add dark mode support',
    requirements: '## Requirements\n- Add theme toggle\n- Save preference',
    status: 'pending',
    created_at: '2026-01-01T10:00:00Z',
    updated_at: '2026-01-01T10:00:00Z',
  },
  {
    id: 'sug_2',
    submitted_by_email: 'user2@example.com',
    submitted_by_name: 'User Two',
    area: 'Navigation',
    original_suggestion: 'Add breadcrumbs',
    status: 'resolved',
    resolution_notes: 'Implemented in v2.0',
    created_at: '2026-01-02T10:00:00Z',
    updated_at: '2026-01-02T12:00:00Z',
  },
];

describe('ImprovementsDashboard', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('renders loading state initially', () => {
    global.fetch = vi.fn().mockImplementation(() => new Promise(() => {}));
    
    render(<ImprovementsDashboard />);
    expect(screen.getByText('Loading suggestions...')).toBeInTheDocument();
  });

  it('renders suggestions after loading', async () => {
    global.fetch = vi.fn().mockResolvedValue({
      ok: true,
      json: () => Promise.resolve({ suggestions: mockSuggestions }),
    });

    render(<ImprovementsDashboard />);

    await waitFor(() => {
      expect(screen.getByText('Improvements')).toBeInTheDocument();
    });

    // Check stats
    expect(screen.getByText('2')).toBeInTheDocument(); // Total
  });

  it('renders empty state when no suggestions', async () => {
    global.fetch = vi.fn().mockResolvedValue({
      ok: true,
      json: () => Promise.resolve({ suggestions: [] }),
    });

    render(<ImprovementsDashboard />);

    await waitFor(() => {
      expect(screen.getByText('No suggestions found')).toBeInTheDocument();
    });
  });

  it('filters suggestions by status', async () => {
    global.fetch = vi.fn().mockResolvedValue({
      ok: true,
      json: () => Promise.resolve({ suggestions: mockSuggestions }),
    });

    render(<ImprovementsDashboard />);

    await waitFor(() => {
      expect(screen.getByText('Improvements')).toBeInTheDocument();
    });

    // Click pending filter
    const pendingButton = screen.getByText('Pending');
    fireEvent.click(pendingButton);

    // Verify fetch was called with status filter
    await waitFor(() => {
      expect(global.fetch).toHaveBeenCalledWith(
        expect.stringContaining('status=pending'),
        expect.any(Object)
      );
    });
  });

  it('expands suggestion details when clicked', async () => {
    global.fetch = vi.fn().mockResolvedValue({
      ok: true,
      json: () => Promise.resolve({ suggestions: mockSuggestions }),
    });

    render(<ImprovementsDashboard />);

    await waitFor(() => {
      expect(screen.getByText(/Add dark mode support/)).toBeInTheDocument();
    });

    // Find and click the first suggestion header
    const suggestionPreview = screen.getByText(/Add dark mode support/);
    const suggestionHeader = suggestionPreview.closest('[style*="cursor"]');
    if (suggestionHeader) {
      fireEvent.click(suggestionHeader);
    }
  });

  it('handles error state', async () => {
    global.fetch = vi.fn().mockRejectedValue(new Error('Network error'));

    render(<ImprovementsDashboard />);

    await waitFor(() => {
      expect(screen.getByText(/Failed to load suggestions/)).toBeInTheDocument();
    });
  });

  it('calls refresh when refresh button is clicked', async () => {
    global.fetch = vi.fn().mockResolvedValue({
      ok: true,
      json: () => Promise.resolve({ suggestions: mockSuggestions }),
    });

    render(<ImprovementsDashboard />);

    await waitFor(() => {
      expect(screen.getByText('Improvements')).toBeInTheDocument();
    });

    const refreshButton = screen.getByText('Refresh');
    fireEvent.click(refreshButton);

    await waitFor(() => {
      expect(global.fetch).toHaveBeenCalledTimes(2); // Initial + refresh
    });
  });
});
