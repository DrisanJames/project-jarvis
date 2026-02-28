import React, { useState, useCallback, useRef } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faUpload,
  faFileAlt,
  faCheck,
  faExclamationTriangle,
  faTimes,
  faSpinner,
  faCloudUploadAlt,
  faCheckCircle,
  faTimesCircle,
  faArrowRight,
  faArrowLeft,
  faQuestionCircle,
} from '@fortawesome/free-solid-svg-icons';

// =============================================================================
// TYPES
// =============================================================================

interface FieldMapping {
  csv_column: string;
  system_field: string;
  is_custom: boolean;
}

interface SuggestedMapping {
  csv_column: string;
  suggested_field: string;
  confidence: number;
  is_custom: boolean;
}

interface HeaderValidationResult {
  valid: boolean;
  has_headers: boolean;
  headers?: string[];
  suggested_mappings?: SuggestedMapping[];
  sample_rows?: string[][];
  total_columns?: number;
  confidence?: number;
  detection_method?: string;
  rejection_reason?: string;
}

interface UploadSession {
  session_id: string;
  chunk_size: number;
  total_chunks: number;
  expires_at: string;
  upload_url: string;
  complete_url: string;
  progress_url: string;
}

interface UploadProgress {
  status: string;
  phase: string;
  processed_rows: number;
  total_rows: number;
  imported_count: number;
  skipped_count: number;
  error_count: number;
  errors: string[];
  duration_ms: number;
}

interface SystemField {
  key: string;
  label: string;
  required: boolean;
  description: string;
  example: string;
  data_type: string;
  category: string;
}

interface ChunkedUploaderProps {
  listId: string;
  listName?: string;
  onComplete?: (result: UploadProgress) => void;
  onCancel?: () => void;
}

// =============================================================================
// CONSTANTS
// =============================================================================

const CHUNK_SIZE = 10 * 1024 * 1024; // 10MB chunks
const MAX_DIRECT_UPLOAD_SIZE = 100 * 1024 * 1024; // 100MB - use direct upload below this

// =============================================================================
// MAIN COMPONENT
// =============================================================================

export const ChunkedUploader: React.FC<ChunkedUploaderProps> = ({
  listId,
  listName,
  onComplete,
  onCancel,
}) => {
  // Step management
  const [step, setStep] = useState<'select' | 'validate' | 'mapping' | 'uploading' | 'processing' | 'complete'>('select');
  
  // File state
  const [file, setFile] = useState<File | null>(null);
  const [isDragging, setIsDragging] = useState(false);
  
  // Validation state
  const [validationResult, setValidationResult] = useState<HeaderValidationResult | null>(null);
  const [systemFields, setSystemFields] = useState<SystemField[]>([]);
  
  // Mapping state
  const [fieldMappings, setFieldMappings] = useState<Record<string, string>>({});
  
  // Upload state
  const [, setUploadSession] = useState<UploadSession | null>(null);
  const [uploadProgress, setUploadProgress] = useState<number>(0);
  const [uploadedChunks, setUploadedChunks] = useState<number>(0);
  const [totalChunks, setTotalChunks] = useState<number>(0);
  
  // Processing state
  const [processingProgress, setProcessingProgress] = useState<UploadProgress | null>(null);
  
  // Error/loading state
  const [error, setError] = useState<string | null>(null);
  const [, setLoading] = useState(false);
  
  // Refs
  const fileInputRef = useRef<HTMLInputElement>(null);
  const abortControllerRef = useRef<AbortController | null>(null);

  // =============================================================================
  // FILE SELECTION
  // =============================================================================

  const handleFileSelect = useCallback(async (selectedFile: File) => {
    setError(null);
    
    // Validate file type
    if (!selectedFile.name.endsWith('.csv') && selectedFile.type !== 'text/csv') {
      setError('Please select a CSV file');
      return;
    }
    
    setFile(selectedFile);
    setStep('validate');
    setLoading(true);
    
    try {
      // Load system fields
      const fieldsRes = await fetch('/api/mailing/lists/upload/fields');
      if (fieldsRes.ok) {
        const data = await fieldsRes.json();
        setSystemFields(data.system_fields || []);
      }
      
      // Read first 10KB for header validation
      const reader = new FileReader();
      const slice = selectedFile.slice(0, 10240); // First 10KB
      
      reader.onload = async (e) => {
        const content = e.target?.result as string;
        
        // Validate headers with Jarvis's API
        const validateRes = await fetch('/api/mailing/lists/upload/validate', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ content }),
        });
        
        const result: HeaderValidationResult = await validateRes.json();
        setValidationResult(result);
        
        if (!result.valid || !result.has_headers) {
          setError(result.rejection_reason || 'No headers detected. CSV files must have a header row.');
          setStep('select');
        } else {
          // Initialize field mappings from suggestions
          const initialMappings: Record<string, string> = {};
          result.suggested_mappings?.forEach((mapping) => {
            if (mapping.suggested_field && mapping.confidence > 0.5) {
              initialMappings[mapping.csv_column] = mapping.suggested_field;
            }
          });
          setFieldMappings(initialMappings);
          setStep('mapping');
        }
        setLoading(false);
      };
      
      reader.onerror = () => {
        setError('Failed to read file');
        setLoading(false);
        setStep('select');
      };
      
      reader.readAsText(slice);
    } catch (err) {
      setError('Failed to validate file');
      setLoading(false);
      setStep('select');
    }
  }, []);

  // Drag and drop handlers
  const handleDragOver = useCallback((e: React.DragEvent) => {
    e.preventDefault();
    setIsDragging(true);
  }, []);

  const handleDragLeave = useCallback((e: React.DragEvent) => {
    e.preventDefault();
    setIsDragging(false);
  }, []);

  const handleDrop = useCallback((e: React.DragEvent) => {
    e.preventDefault();
    setIsDragging(false);
    const droppedFile = e.dataTransfer.files[0];
    if (droppedFile) {
      handleFileSelect(droppedFile);
    }
  }, [handleFileSelect]);

  // =============================================================================
  // FIELD MAPPING
  // =============================================================================

  const updateMapping = (csvColumn: string, systemField: string) => {
    setFieldMappings(prev => ({
      ...prev,
      [csvColumn]: systemField,
    }));
  };

  const hasEmailMapping = () => {
    return Object.values(fieldMappings).includes('email');
  };

  // =============================================================================
  // UPLOAD LOGIC
  // =============================================================================

  const startUpload = async () => {
    if (!file || !hasEmailMapping()) return;
    
    setError(null);
    setStep('uploading');
    setLoading(true);
    
    // Build field mapping array
    const mappingArray: FieldMapping[] = Object.entries(fieldMappings)
      .filter(([_, systemField]) => systemField && systemField !== 'ignore')
      .map(([csvColumn, systemField]) => ({
        csv_column: csvColumn,
        system_field: systemField,
        is_custom: systemField.startsWith('custom_'),
      }));
    
    try {
      if (file.size <= MAX_DIRECT_UPLOAD_SIZE) {
        // Direct upload for smaller files
        await performDirectUpload(mappingArray);
      } else {
        // Chunked upload for large files
        await performChunkedUpload(mappingArray);
      }
    } catch (err: any) {
      setError(err.message || 'Upload failed');
      setStep('mapping');
      setLoading(false);
    }
  };

  const performDirectUpload = async (mappingArray: FieldMapping[]) => {
    if (!file) return;
    
    const formData = new FormData();
    formData.append('file', file);
    formData.append('field_mapping', JSON.stringify(mappingArray));
    formData.append('update_existing', 'true');
    
    const res = await fetch(`/api/mailing/lists/${listId}/upload`, {
      method: 'POST',
      body: formData,
    });
    
    if (!res.ok) {
      const errorData = await res.json();
      throw new Error(errorData.error || 'Upload failed');
    }
    
    const result = await res.json();
    setProcessingProgress(result);
    setStep('complete');
    setLoading(false);
    onComplete?.(result);
  };

  const performChunkedUpload = async (mappingArray: FieldMapping[]) => {
    if (!file) return;
    
    abortControllerRef.current = new AbortController();
    
    // Step 1: Initialize upload session
    const initRes = await fetch(`/api/mailing/lists/${listId}/upload/init`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        filename: file.name,
        file_size: file.size,
        chunk_size: CHUNK_SIZE,
      }),
      signal: abortControllerRef.current.signal,
    });
    
    if (!initRes.ok) {
      const errorData = await initRes.json();
      throw new Error(errorData.error || 'Failed to initialize upload');
    }
    
    const session: UploadSession = await initRes.json();
    setUploadSession(session);
    setTotalChunks(session.total_chunks);
    setUploadedChunks(0);
    
    // Step 2: Upload chunks
    const chunkSize = session.chunk_size;
    const chunks = Math.ceil(file.size / chunkSize);
    
    for (let i = 0; i < chunks; i++) {
      if (abortControllerRef.current?.signal.aborted) {
        throw new Error('Upload cancelled');
      }
      
      const start = i * chunkSize;
      const end = Math.min(start + chunkSize, file.size);
      const chunk = file.slice(start, end);
      
      const chunkRes = await fetch(
        `/api/mailing/lists/${listId}/upload/${session.session_id}/chunk/${i}`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/octet-stream' },
          body: chunk,
          signal: abortControllerRef.current.signal,
        }
      );
      
      if (!chunkRes.ok) {
        const errorData = await chunkRes.json();
        throw new Error(errorData.error || `Chunk ${i} upload failed`);
      }
      
      setUploadedChunks(i + 1);
      setUploadProgress(((i + 1) / chunks) * 100);
    }
    
    // Step 3: Complete upload and start processing
    setStep('processing');
    
    const completeRes = await fetch(
      `/api/mailing/lists/${listId}/upload/${session.session_id}/complete`,
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          field_mapping: mappingArray,
          update_existing: true,
        }),
        signal: abortControllerRef.current.signal,
      }
    );
    
    if (!completeRes.ok) {
      const errorData = await completeRes.json();
      throw new Error(errorData.error || 'Failed to complete upload');
    }
    
    // Step 4: Poll for processing progress
    await pollProcessingProgress(session.session_id);
  };

  const pollProcessingProgress = async (sessionId: string) => {
    const pollInterval = 1000; // 1 second
    const maxAttempts = 3600; // Max 1 hour
    
    for (let attempt = 0; attempt < maxAttempts; attempt++) {
      if (abortControllerRef.current?.signal.aborted) {
        throw new Error('Processing cancelled');
      }
      
      try {
        const progressRes = await fetch(
          `/api/mailing/lists/${listId}/upload/${sessionId}/progress`,
          { signal: abortControllerRef.current?.signal }
        );
        
        if (progressRes.ok) {
          const progress: UploadProgress = await progressRes.json();
          setProcessingProgress(progress);
          
          if (progress.status === 'completed' || progress.status === 'failed') {
            setStep('complete');
            setLoading(false);
            onComplete?.(progress);
            return;
          }
        }
      } catch (err) {
        // Ignore poll errors, just keep trying
      }
      
      await new Promise(resolve => setTimeout(resolve, pollInterval));
    }
    
    throw new Error('Processing timed out');
  };

  const cancelUpload = () => {
    abortControllerRef.current?.abort();
    setStep('select');
    setFile(null);
    setLoading(false);
  };

  // =============================================================================
  // UTILITY FUNCTIONS
  // =============================================================================

  const formatFileSize = (bytes: number): string => {
    if (bytes < 1024) return `${bytes} B`;
    if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
    if (bytes < 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
    return `${(bytes / (1024 * 1024 * 1024)).toFixed(2)} GB`;
  };

  const getProgressPercentage = (): number => {
    if (step === 'uploading') return uploadProgress;
    if (step === 'processing' && processingProgress) {
      if (processingProgress.total_rows > 0) {
        return (processingProgress.processed_rows / processingProgress.total_rows) * 100;
      }
    }
    return 0;
  };

  // Group system fields by category
  const groupedFields = systemFields.reduce((acc, field) => {
    if (!acc[field.category]) acc[field.category] = [];
    acc[field.category].push(field);
    return acc;
  }, {} as Record<string, SystemField[]>);

  // =============================================================================
  // RENDER
  // =============================================================================

  return (
    <div className="chunked-uploader">
      {/* Header */}
      <div className="cu-header">
        <h2>Import Subscribers</h2>
        {listName && <p>Importing to: <strong>{listName}</strong></p>}
        {onCancel && (
          <button className="cu-close" onClick={onCancel}>
            <FontAwesomeIcon icon={faTimes} />
          </button>
        )}
      </div>

      {/* Progress Steps */}
      <div className="cu-steps">
        {['Select File', 'Validate', 'Map Fields', 'Upload', 'Complete'].map((label, idx) => {
          const stepKeys = ['select', 'validate', 'mapping', 'uploading', 'complete'];
          const currentIdx = stepKeys.indexOf(step === 'processing' ? 'uploading' : step);
          const isComplete = idx < currentIdx || step === 'complete';
          const isCurrent = idx === currentIdx;

          return (
            <div key={label} className={`cu-step ${isComplete ? 'complete' : ''} ${isCurrent ? 'current' : ''}`}>
              <div className="cu-step-indicator">
                {isComplete ? <FontAwesomeIcon icon={faCheck} size="xs" /> : idx + 1}
              </div>
              <span>{label}</span>
            </div>
          );
        })}
      </div>

      {/* Error Banner */}
      {error && (
        <div className="cu-error">
          <FontAwesomeIcon icon={faExclamationTriangle} />
          <span>{error}</span>
          <button onClick={() => setError(null)}><FontAwesomeIcon icon={faTimes} /></button>
        </div>
      )}

      {/* Step: Select File */}
      {step === 'select' && (
        <div className="cu-select">
          <div className="cu-section-header">
            <h3>Select Your CSV File</h3>
            <p>Upload a CSV file with subscriber data. Files up to <strong>10 GB</strong> are supported.</p>
          </div>

          <div
            className={`cu-dropzone ${isDragging ? 'dragging' : ''}`}
            onDragOver={handleDragOver}
            onDragLeave={handleDragLeave}
            onDrop={handleDrop}
          >
            <FontAwesomeIcon icon={faCloudUploadAlt} size="3x" className="cu-dropzone-icon" />
            <h4>Drag & drop your CSV file here</h4>
            <p>or</p>
            <label className="cu-file-input">
              <input
                ref={fileInputRef}
                type="file"
                accept=".csv,text/csv"
                onChange={(e) => e.target.files?.[0] && handleFileSelect(e.target.files[0])}
              />
              <span>
                <FontAwesomeIcon icon={faUpload} /> Browse Files
              </span>
            </label>
            <p className="cu-file-hint">Supports CSV files up to 10 GB</p>
          </div>

          <div className="cu-requirements">
            <h4><FontAwesomeIcon icon={faQuestionCircle} /> File Requirements</h4>
            <ul>
              <li><strong>Format:</strong> CSV (comma-separated values)</li>
              <li><strong>Headers:</strong> First row must contain column names</li>
              <li><strong>Required:</strong> Must include an "email" column</li>
              <li><strong>Encoding:</strong> UTF-8 recommended</li>
            </ul>
          </div>
        </div>
      )}

      {/* Step: Validate (loading) */}
      {step === 'validate' && (
        <div className="cu-validate">
          <div className="cu-loading-container">
            <FontAwesomeIcon icon={faSpinner} spin size="3x" className="cu-spinner" />
            <h3>Validating File...</h3>
            <p>Detecting headers and analyzing your CSV structure</p>
            {file && <p className="cu-file-name">{file.name} ({formatFileSize(file.size)})</p>}
          </div>
        </div>
      )}

      {/* Step: Mapping */}
      {step === 'mapping' && validationResult && (
        <div className="cu-mapping">
          <div className="cu-section-header">
            <h3>Map Your Columns</h3>
            <p>Match your CSV columns to subscriber fields. We've suggested mappings based on your headers.</p>
          </div>

          {file && (
            <div className="cu-file-info">
              <FontAwesomeIcon icon={faFileAlt} />
              <span>{file.name}</span>
              <span className="cu-file-size">({formatFileSize(file.size)})</span>
              <button onClick={() => { setFile(null); setStep('select'); }}>Change File</button>
            </div>
          )}

          <div className="cu-mapping-table-wrapper">
            <table className="cu-mapping-table">
              <thead>
                <tr>
                  <th>Your Column</th>
                  <th>Maps To</th>
                  <th>Sample Data</th>
                </tr>
              </thead>
              <tbody>
                {validationResult.headers?.map((header, idx) => (
                  <tr key={idx} className={fieldMappings[header] === 'email' ? 'cu-email-row' : ''}>
                    <td>
                      <strong>{header}</strong>
                      {(validationResult.suggested_mappings?.[idx]?.confidence ?? 0) > 0.8 && (
                        <FontAwesomeIcon icon={faCheckCircle} className="cu-confidence-high" />
                      )}
                    </td>
                    <td>
                      <select
                        value={fieldMappings[header] || ''}
                        onChange={(e) => updateMapping(header, e.target.value)}
                      >
                        <option value="">-- Ignore this column --</option>
                        {Object.entries(groupedFields).map(([category, fields]) => (
                          <optgroup key={category} label={category.charAt(0).toUpperCase() + category.slice(1)}>
                            {fields.map((field) => (
                              <option key={field.key} value={field.key}>
                                {field.label} {field.required && '*'}
                              </option>
                            ))}
                          </optgroup>
                        ))}
                        <optgroup label="Custom Field">
                          <option value={`custom_${header.toLowerCase().replace(/\s+/g, '_')}`}>
                            Custom: {header}
                          </option>
                        </optgroup>
                      </select>
                    </td>
                    <td className="cu-preview-cell">
                      {validationResult.sample_rows?.[0]?.[idx] || <em>empty</em>}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>

          {/* Data Preview */}
          {validationResult.sample_rows && validationResult.sample_rows.length > 1 && (
            <div className="cu-data-preview">
              <h4>Data Preview (first {validationResult.sample_rows.length} rows)</h4>
              <div className="cu-preview-scroll">
                <table>
                  <thead>
                    <tr>
                      {validationResult.headers?.map((h, i) => (
                        <th key={i}>{h}</th>
                      ))}
                    </tr>
                  </thead>
                  <tbody>
                    {validationResult.sample_rows.map((row, ri) => (
                      <tr key={ri}>
                        {row.map((cell, ci) => (
                          <td key={ci}>{cell || <em>—</em>}</td>
                        ))}
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </div>
          )}

          <div className="cu-actions">
            <button className="cu-btn" onClick={() => { setStep('select'); setFile(null); }}>
              <FontAwesomeIcon icon={faArrowLeft} /> Back
            </button>
            <button
              className="cu-btn cu-btn-primary"
              onClick={startUpload}
              disabled={!hasEmailMapping()}
            >
              Start Import <FontAwesomeIcon icon={faArrowRight} />
            </button>
          </div>
          
          {!hasEmailMapping() && (
            <p className="cu-mapping-warning">
              <FontAwesomeIcon icon={faExclamationTriangle} /> You must map at least one column to "Email Address"
            </p>
          )}
        </div>
      )}

      {/* Step: Uploading */}
      {(step === 'uploading' || step === 'processing') && (
        <div className="cu-uploading">
          <div className="cu-progress-container">
            <FontAwesomeIcon 
              icon={faSpinner} 
              spin 
              size="3x" 
              className="cu-spinner" 
            />
            
            <h3>{step === 'uploading' ? 'Uploading File...' : 'Processing Subscribers...'}</h3>
            
            <div className="cu-progress-bar-container">
              <div 
                className="cu-progress-bar"
                style={{ width: `${getProgressPercentage()}%` }}
              />
            </div>
            
            <p className="cu-progress-text">
              {step === 'uploading' ? (
                <>Uploading chunk {uploadedChunks} of {totalChunks} ({Math.round(uploadProgress)}%)</>
              ) : processingProgress ? (
                <>Processed {processingProgress.processed_rows.toLocaleString()} of {processingProgress.total_rows.toLocaleString()} rows</>
              ) : (
                'Initializing...'
              )}
            </p>
            
            {file && (
              <p className="cu-file-name">{file.name} ({formatFileSize(file.size)})</p>
            )}
            
            <button className="cu-btn cu-btn-danger" onClick={cancelUpload}>
              <FontAwesomeIcon icon={faTimes} /> Cancel
            </button>
          </div>
        </div>
      )}

      {/* Step: Complete */}
      {step === 'complete' && processingProgress && (
        <div className="cu-complete">
          <div className={`cu-complete-icon ${processingProgress.status === 'failed' ? 'error' : ''}`}>
            <FontAwesomeIcon 
              icon={processingProgress.status === 'failed' ? faTimesCircle : faCheckCircle} 
              size="3x" 
            />
          </div>
          
          <h3>{processingProgress.status === 'failed' ? 'Import Failed' : 'Import Complete!'}</h3>
          
          <div className="cu-stats-grid">
            <div className="cu-stat">
              <span className="cu-stat-value">{processingProgress.total_rows.toLocaleString()}</span>
              <span className="cu-stat-label">Total Rows</span>
            </div>
            <div className="cu-stat success">
              <span className="cu-stat-value">{processingProgress.imported_count.toLocaleString()}</span>
              <span className="cu-stat-label">Imported</span>
            </div>
            <div className="cu-stat warning">
              <span className="cu-stat-value">{processingProgress.skipped_count.toLocaleString()}</span>
              <span className="cu-stat-label">Skipped</span>
            </div>
            <div className="cu-stat error">
              <span className="cu-stat-value">{processingProgress.error_count.toLocaleString()}</span>
              <span className="cu-stat-label">Errors</span>
            </div>
          </div>

          {processingProgress.duration_ms > 0 && (
            <p className="cu-duration">
              Completed in {(processingProgress.duration_ms / 1000).toFixed(1)} seconds
            </p>
          )}

          {processingProgress.errors && processingProgress.errors.length > 0 && (
            <div className="cu-error-list">
              <h4>Errors:</h4>
              <ul>
                {processingProgress.errors.slice(0, 10).map((err, i) => (
                  <li key={i}>{err}</li>
                ))}
                {processingProgress.errors.length > 10 && (
                  <li>...and {processingProgress.errors.length - 10} more</li>
                )}
              </ul>
            </div>
          )}

          <div className="cu-actions">
            <button className="cu-btn cu-btn-primary" onClick={onCancel}>
              Done
            </button>
            <button className="cu-btn" onClick={() => { setStep('select'); setFile(null); setProcessingProgress(null); }}>
              Import More
            </button>
          </div>
        </div>
      )}

      <style>{`
        .chunked-uploader {
          background: white;
          border-radius: 12px;
          max-width: 900px;
          margin: 0 auto;
        }

        .cu-header {
          display: flex;
          justify-content: space-between;
          align-items: center;
          padding: 20px 24px;
          border-bottom: 1px solid #e5e7eb;
        }

        .cu-header h2 { margin: 0; font-size: 20px; color: #111827; }
        .cu-header p { margin: 4px 0 0; color: #6b7280; font-size: 14px; }
        .cu-close { 
          background: none; 
          border: none; 
          cursor: pointer; 
          color: #9ca3af;
          padding: 8px;
          border-radius: 6px;
        }
        .cu-close:hover { background: #f3f4f6; color: #6b7280; }

        .cu-steps {
          display: flex;
          justify-content: center;
          gap: 24px;
          padding: 20px;
          background: #f9fafb;
          flex-wrap: wrap;
        }

        .cu-step {
          display: flex;
          align-items: center;
          gap: 8px;
          color: #9ca3af;
          font-size: 13px;
        }

        .cu-step.current { color: #3b82f6; font-weight: 500; }
        .cu-step.complete { color: #10b981; }

        .cu-step-indicator {
          width: 24px;
          height: 24px;
          border-radius: 50%;
          background: #e5e7eb;
          display: flex;
          align-items: center;
          justify-content: center;
          font-size: 12px;
          font-weight: 600;
        }

        .cu-step.current .cu-step-indicator { background: #3b82f6; color: white; }
        .cu-step.complete .cu-step-indicator { background: #10b981; color: white; }

        .cu-error {
          display: flex;
          align-items: center;
          gap: 10px;
          padding: 12px 24px;
          background: #fef2f2;
          border-bottom: 1px solid #fecaca;
          color: #dc2626;
          font-size: 14px;
        }

        .cu-error button {
          margin-left: auto;
          background: none;
          border: none;
          cursor: pointer;
          color: #dc2626;
          padding: 4px 8px;
        }

        .cu-section-header {
          padding: 24px 24px 16px;
        }

        .cu-section-header h3 { margin: 0 0 4px; font-size: 18px; color: #111827; }
        .cu-section-header p { margin: 0; color: #6b7280; font-size: 14px; }

        /* File Selection */
        .cu-select { padding: 0 24px 24px; }

        .cu-dropzone {
          display: flex;
          flex-direction: column;
          align-items: center;
          justify-content: center;
          padding: 48px;
          border: 2px dashed #d1d5db;
          border-radius: 12px;
          background: #f9fafb;
          text-align: center;
          transition: all 0.2s;
        }

        .cu-dropzone.dragging {
          border-color: #3b82f6;
          background: #eff6ff;
        }

        .cu-dropzone-icon { color: #9ca3af; margin-bottom: 16px; }
        .cu-dropzone h4 { margin: 0 0 8px; color: #374151; }
        .cu-dropzone > p { margin: 0 0 16px; color: #6b7280; }
        .cu-file-hint { font-size: 12px; color: #9ca3af; margin-top: 16px !important; }

        .cu-file-input input { display: none; }
        .cu-file-input span {
          display: inline-flex;
          align-items: center;
          gap: 8px;
          padding: 10px 20px;
          background: #3b82f6;
          color: white;
          border-radius: 6px;
          cursor: pointer;
          font-weight: 500;
          transition: background 0.2s;
        }

        .cu-file-input span:hover { background: #2563eb; }

        .cu-requirements {
          margin-top: 24px;
          padding: 16px;
          background: #fffbeb;
          border-radius: 8px;
        }

        .cu-requirements h4 {
          display: flex;
          align-items: center;
          gap: 8px;
          margin: 0 0 12px;
          font-size: 14px;
          color: #92400e;
        }

        .cu-requirements ul {
          margin: 0;
          padding-left: 20px;
          font-size: 13px;
          color: #78716c;
        }

        .cu-requirements li { margin-bottom: 4px; }

        /* Validation Loading */
        .cu-validate { padding: 48px 24px; }
        
        .cu-loading-container {
          text-align: center;
        }

        .cu-spinner { color: #3b82f6; margin-bottom: 16px; }
        .cu-loading-container h3 { margin: 0 0 8px; color: #111827; }
        .cu-loading-container p { margin: 0; color: #6b7280; }
        .cu-file-name { color: #9ca3af; font-size: 13px; margin-top: 16px; }

        /* Mapping */
        .cu-mapping { padding: 0 24px 24px; }

        .cu-file-info {
          display: flex;
          align-items: center;
          gap: 8px;
          padding: 12px 16px;
          background: #f0f9ff;
          border-radius: 8px;
          font-size: 14px;
          color: #1e40af;
        }

        .cu-file-size { color: #6b7280; }
        .cu-file-info button {
          margin-left: auto;
          background: none;
          border: none;
          color: #3b82f6;
          cursor: pointer;
          font-size: 13px;
        }

        .cu-mapping-table-wrapper {
          margin-top: 20px;
          border: 1px solid #e5e7eb;
          border-radius: 8px;
          overflow: hidden;
        }

        .cu-mapping-table {
          width: 100%;
          border-collapse: collapse;
        }

        .cu-mapping-table th,
        .cu-mapping-table td {
          padding: 12px;
          text-align: left;
          border-bottom: 1px solid #e5e7eb;
        }

        .cu-mapping-table th {
          background: #f9fafb;
          font-size: 11px;
          font-weight: 600;
          color: #6b7280;
          text-transform: uppercase;
        }

        .cu-mapping-table td { font-size: 14px; color: #374151; }

        .cu-mapping-table select {
          width: 100%;
          padding: 8px;
          border: 1px solid #d1d5db;
          border-radius: 6px;
          font-size: 14px;
          background: white;
        }

        .cu-email-row { background: #f0fdf4; }

        .cu-confidence-high { color: #10b981; margin-left: 8px; }

        .cu-preview-cell {
          max-width: 150px;
          overflow: hidden;
          text-overflow: ellipsis;
          white-space: nowrap;
          color: #6b7280;
          font-size: 13px;
        }

        .cu-data-preview { margin-top: 20px; }
        .cu-data-preview h4 { margin: 0 0 12px; font-size: 14px; color: #374151; }

        .cu-preview-scroll {
          overflow-x: auto;
          border: 1px solid #e5e7eb;
          border-radius: 8px;
        }

        .cu-preview-scroll table {
          width: 100%;
          border-collapse: collapse;
          font-size: 13px;
        }

        .cu-preview-scroll th,
        .cu-preview-scroll td {
          padding: 8px 12px;
          border-bottom: 1px solid #e5e7eb;
          white-space: nowrap;
        }

        .cu-preview-scroll th { background: #f9fafb; font-weight: 500; color: #374151; }

        .cu-mapping-warning {
          text-align: center;
          color: #f59e0b;
          font-size: 13px;
          margin-top: 12px;
        }

        /* Uploading/Processing */
        .cu-uploading { padding: 48px 24px; }

        .cu-progress-container {
          text-align: center;
        }

        .cu-progress-container h3 { margin: 20px 0 8px; color: #111827; }
        .cu-progress-container p { color: #6b7280; margin: 0; }

        .cu-progress-bar-container {
          width: 100%;
          max-width: 400px;
          height: 8px;
          background: #e5e7eb;
          border-radius: 4px;
          margin: 24px auto;
          overflow: hidden;
        }

        .cu-progress-bar {
          height: 100%;
          background: linear-gradient(90deg, #3b82f6, #60a5fa);
          transition: width 0.3s;
        }

        .cu-progress-text { color: #6b7280; font-size: 14px; margin-top: 8px; }

        /* Complete */
        .cu-complete { padding: 48px 24px; text-align: center; }

        .cu-complete-icon {
          width: 80px;
          height: 80px;
          margin: 0 auto 20px;
          background: #dcfce7;
          border-radius: 50%;
          display: flex;
          align-items: center;
          justify-content: center;
          color: #10b981;
        }

        .cu-complete-icon.error {
          background: #fee2e2;
          color: #ef4444;
        }

        .cu-complete h3 { margin: 0 0 24px; color: #111827; }

        .cu-stats-grid {
          display: grid;
          grid-template-columns: repeat(4, 1fr);
          gap: 16px;
          max-width: 500px;
          margin: 0 auto 24px;
        }

        .cu-stat {
          padding: 16px;
          background: #f9fafb;
          border-radius: 8px;
        }

        .cu-stat-value { display: block; font-size: 24px; font-weight: 700; color: #111827; }
        .cu-stat-label { font-size: 11px; color: #6b7280; text-transform: uppercase; }
        .cu-stat.success .cu-stat-value { color: #10b981; }
        .cu-stat.warning .cu-stat-value { color: #f59e0b; }
        .cu-stat.error .cu-stat-value { color: #ef4444; }

        .cu-duration { color: #6b7280; font-size: 13px; margin-bottom: 16px; }

        .cu-error-list {
          max-width: 500px;
          margin: 0 auto 24px;
          text-align: left;
          background: #fef2f2;
          padding: 16px;
          border-radius: 8px;
        }

        .cu-error-list h4 { margin: 0 0 8px; color: #dc2626; font-size: 14px; }
        .cu-error-list ul { margin: 0; padding-left: 20px; font-size: 13px; color: #991b1b; }

        /* Actions */
        .cu-actions {
          display: flex;
          justify-content: center;
          gap: 12px;
          padding-top: 20px;
          border-top: 1px solid #e5e7eb;
          margin-top: 20px;
        }

        .cu-btn {
          display: inline-flex;
          align-items: center;
          gap: 8px;
          padding: 10px 20px;
          border: 1px solid #d1d5db;
          border-radius: 6px;
          background: white;
          font-size: 14px;
          font-weight: 500;
          cursor: pointer;
          transition: all 0.2s;
          color: #374151;
        }

        .cu-btn:hover { background: #f9fafb; }
        .cu-btn:disabled { opacity: 0.5; cursor: not-allowed; }

        .cu-btn-primary {
          background: #3b82f6;
          border-color: #3b82f6;
          color: white;
        }

        .cu-btn-primary:hover { background: #2563eb; }
        .cu-btn-primary:disabled { background: #93c5fd; border-color: #93c5fd; }

        .cu-btn-danger {
          background: white;
          border-color: #fca5a5;
          color: #dc2626;
        }

        .cu-btn-danger:hover { background: #fef2f2; }

        @media (max-width: 600px) {
          .cu-stats-grid { grid-template-columns: repeat(2, 1fr); }
          .cu-steps { gap: 12px; }
          .cu-step span { display: none; }
        }
      `}</style>
    </div>
  );
};

export default ChunkedUploader;
