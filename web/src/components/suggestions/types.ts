// Suggestion types

export type SuggestionStatus = 'pending' | 'resolved' | 'denied';

export interface Suggestion {
  id: string;
  submitted_by_email: string;
  submitted_by_name: string;
  area: string;
  area_context?: string;
  original_suggestion: string;
  requirements?: string;
  status: SuggestionStatus;
  resolution_notes?: string;
  created_at: string;
  updated_at: string;
}

export interface CreateSuggestionRequest {
  area: string;
  area_context?: string;
  original_suggestion: string;
}

export interface UpdateSuggestionStatusRequest {
  status: 'resolved' | 'denied';
  resolution_notes?: string;
}

export interface SuggestionsResponse {
  suggestions: Suggestion[];
}

export interface SuggestionResponse {
  success: boolean;
  suggestion: Suggestion;
}
