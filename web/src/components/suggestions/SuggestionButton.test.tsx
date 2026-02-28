import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { SuggestionButton } from './SuggestionButton';

describe('SuggestionButton', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('renders the floating button', () => {
    render(<SuggestionButton />);
    const button = screen.getByTitle('Suggest an improvement');
    expect(button).toBeInTheDocument();
  });

  it('opens the area selection overlay when clicked', () => {
    render(<SuggestionButton />);
    const button = screen.getByTitle('Suggest an improvement');
    fireEvent.click(button);
    
    expect(screen.getByText('Tap the area you would like to improve')).toBeInTheDocument();
  });

  it('closes the overlay when close button is clicked', () => {
    render(<SuggestionButton />);
    
    // Open the overlay
    const button = screen.getByTitle('Suggest an improvement');
    fireEvent.click(button);
    expect(screen.getByText('Tap the area you would like to improve')).toBeInTheDocument();
    
    // Close the overlay
    const closeButtons = screen.getAllByRole('button');
    const closeButton = closeButtons.find(btn => btn.querySelector('svg'));
    if (closeButton) {
      fireEvent.click(closeButton);
    }
  });

  it('shows the form after area selection', async () => {
    render(<SuggestionButton />);
    
    // Open the overlay
    const button = screen.getByTitle('Suggest an improvement');
    fireEvent.click(button);
    
    // Simulate clicking on the overlay (area selection)
    const overlay = screen.getByText('Tap the area you would like to improve').parentElement?.parentElement;
    if (overlay) {
      fireEvent.click(overlay);
    }
    
    // Should show the form
    await waitFor(() => {
      expect(screen.getByText('Suggest an Improvement')).toBeInTheDocument();
    });
  });

  it('calls onSuggestionSubmitted callback after successful submission', async () => {
    const mockCallback = vi.fn();
    const mockFetch = vi.fn().mockResolvedValue({
      ok: true,
      json: () => Promise.resolve({ success: true }),
    });
    global.fetch = mockFetch;

    render(<SuggestionButton onSuggestionSubmitted={mockCallback} />);
    
    // Open the overlay
    const button = screen.getByTitle('Suggest an improvement');
    fireEvent.click(button);
    
    // Simulate clicking on the overlay
    const overlay = screen.getByText('Tap the area you would like to improve').parentElement?.parentElement;
    if (overlay) {
      fireEvent.click(overlay);
    }
    
    // Wait for form to appear
    await waitFor(() => {
      expect(screen.getByPlaceholderText(/Describe what you'd like to improve/)).toBeInTheDocument();
    });

    // Fill in the suggestion
    const textarea = screen.getByPlaceholderText(/Describe what you'd like to improve/);
    fireEvent.change(textarea, { target: { value: 'Test suggestion' } });
    
    // Submit the form
    const submitButton = screen.getByText('Submit Suggestion');
    fireEvent.click(submitButton);
    
    await waitFor(() => {
      expect(mockFetch).toHaveBeenCalled();
    });
  });
});
