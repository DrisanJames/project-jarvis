import React, { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { 
  faDownload, 
  faUpload, 
  faFileAlt, 
  faCheck, 
  faExclamationCircle, 
  faChevronDown, 
  faChevronRight, 
  faTimes, 
  faQuestionCircle 
} from '@fortawesome/free-solid-svg-icons';

// ==========================================
// TYPES
// ==========================================

interface Template {
  id: string;
  name: string;
  description: string;
  fields: number | string;
  download_url: string;
}

interface FieldInfo {
  key: string;
  label: string;
  required: boolean;
  description: string;
  example: string;
  data_type: string;
  category: string;
}

interface ColumnMapping {
  column_index: number;
  original_header: string;
  suggested_field: string | null;
  confidence: string;
  is_custom: boolean;
}

interface ImportJob {
  job_id: string;
  status: string;
  total_rows?: number;
  processed_rows?: number;
  imported_count?: number;
  skipped_count?: number;
  error_count?: number;
}

interface SubscriberImportProps {
  listId: string;
  listName?: string;
  onComplete?: (result: ImportJob) => void;
  onCancel?: () => void;
}

// ==========================================
// MAIN COMPONENT
// ==========================================

export const SubscriberImport: React.FC<SubscriberImportProps> = ({
  listId,
  listName,
  onComplete,
  onCancel,
}) => {
  const [step, setStep] = useState<'templates' | 'upload' | 'mapping' | 'importing' | 'complete'>('templates');
  const [templates, setTemplates] = useState<Template[]>([]);
  const [fields, setFields] = useState<FieldInfo[]>([]);
  const [tips, setTips] = useState<string[]>([]);
  const [file, setFile] = useState<File | null>(null);
  const [headers, setHeaders] = useState<string[]>([]);
  const [previewRows, setPreviewRows] = useState<string[][]>([]);
  const [mapping, setMapping] = useState<ColumnMapping[]>([]);
  const [customMapping, setCustomMapping] = useState<Record<number, string>>({});
  const [importJob, setImportJob] = useState<ImportJob | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [expandedCategories, setExpandedCategories] = useState<Set<string>>(new Set(['required', 'profile']));

  // Load templates and fields on mount
  useEffect(() => {
    const loadData = async () => {
      try {
        const [templatesRes, fieldsRes] = await Promise.all([
          fetch('/api/mailing/import/templates'),
          fetch('/api/mailing/import/fields'),
        ]);

        if (templatesRes.ok) {
          const data = await templatesRes.json();
          setTemplates(data.templates || []);
          setTips(data.tips || []);
        }
        if (fieldsRes.ok) {
          const data = await fieldsRes.json();
          setFields(data.fields || []);
        }
      } catch (err) {
        console.error('Failed to load import data:', err);
      }
    };
    loadData();
  }, []);

  // Handle file selection
  const handleFileSelect = async (selectedFile: File) => {
    setFile(selectedFile);
    setError(null);
    setLoading(true);

    try {
      // Preview the file
      const formData = new FormData();
      formData.append('file', selectedFile);

      const previewRes = await fetch('/api/mailing/import/preview', {
        method: 'POST',
        body: formData,
      });

      if (previewRes.ok) {
        const data = await previewRes.json();
        setHeaders(data.headers);
        setPreviewRows(data.preview_rows);

        // Validate headers and get mapping suggestions
        const validateRes = await fetch('/api/mailing/import/validate', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ headers: data.headers }),
        });

        if (validateRes.ok) {
          const mappingData = await validateRes.json();
          setMapping(mappingData.mapping);
          
          if (!mappingData.validation.has_email) {
            setError('Your file must have an "email" column. Please check your file and try again.');
          } else {
            setStep('mapping');
          }
        }
      } else {
        setError('Failed to read file. Please ensure it is a valid CSV file.');
      }
    } catch (err) {
      setError('Failed to process file. Please try again.');
    }
    setLoading(false);
  };

  // Handle drag and drop
  const handleDrop = useCallback((e: React.DragEvent) => {
    e.preventDefault();
    const droppedFile = e.dataTransfer.files[0];
    if (droppedFile && droppedFile.type === 'text/csv') {
      handleFileSelect(droppedFile);
    } else {
      setError('Please upload a CSV file.');
    }
  }, []);

  // Update column mapping
  const updateMapping = (columnIndex: number, fieldKey: string) => {
    setCustomMapping(prev => ({
      ...prev,
      [columnIndex]: fieldKey,
    }));
  };

  // Start import
  const startImport = async () => {
    if (!file) return;
    
    setLoading(true);
    setStep('importing');

    // Build field mapping object
    const fieldMapping: Record<string, number> = {};
    mapping.forEach((col, idx) => {
      const fieldKey = customMapping[idx] ?? col.suggested_field;
      if (fieldKey && fieldKey !== 'ignore') {
        fieldMapping[fieldKey] = idx;
      }
    });

    try {
      const formData = new FormData();
      formData.append('file', file);
      formData.append('field_mapping', JSON.stringify(fieldMapping));

      const res = await fetch(`/api/mailing/lists/${listId}/import`, {
        method: 'POST',
        body: formData,
      });

      if (res.ok) {
        const job = await res.json();
        setImportJob(job);
        
        // Poll for completion
        pollJobStatus(job.job_id);
      } else {
        setError('Failed to start import. Please try again.');
        setStep('mapping');
      }
    } catch (err) {
      setError('Import failed. Please try again.');
      setStep('mapping');
    }
    setLoading(false);
  };

  // Poll job status
  const pollJobStatus = async (jobId: string) => {
    const poll = async () => {
      try {
        const res = await fetch(`/api/mailing/import-jobs/${jobId}`);
        if (res.ok) {
          const job = await res.json();
          setImportJob(job);

          if (job.status === 'completed' || job.status === 'failed') {
            setStep('complete');
            onComplete?.(job);
          } else {
            setTimeout(poll, 1000);
          }
        }
      } catch (err) {
        console.error('Failed to poll job status');
      }
    };
    
    setTimeout(poll, 1000);
  };

  // Toggle category expansion
  const toggleCategory = (category: string) => {
    setExpandedCategories(prev => {
      const next = new Set(prev);
      if (next.has(category)) {
        next.delete(category);
      } else {
        next.add(category);
      }
      return next;
    });
  };

  // Get final mapping for a column
  const getFinalMapping = (idx: number): string => {
    return customMapping[idx] ?? mapping[idx]?.suggested_field ?? '';
  };

  return (
    <div className="subscriber-import">
      {/* Header */}
      <div className="si-header">
        <h2>Import Subscribers</h2>
        {listName && <p>Importing to: <strong>{listName}</strong></p>}
        {onCancel && (
          <button className="si-close" onClick={onCancel}>
            <FontAwesomeIcon icon={faTimes} />
          </button>
        )}
      </div>

      {/* Progress Steps */}
      <div className="si-steps">
        {['Download Template', 'Upload File', 'Map Fields', 'Import'].map((label, idx) => {
          const stepKeys = ['templates', 'upload', 'mapping', 'importing'];
          const currentIdx = stepKeys.indexOf(step === 'complete' ? 'importing' : step);
          const isComplete = idx < currentIdx || step === 'complete';
          const isCurrent = idx === currentIdx;

          return (
            <div key={label} className={`si-step ${isComplete ? 'complete' : ''} ${isCurrent ? 'current' : ''}`}>
              <div className="si-step-indicator">
                {isComplete ? <FontAwesomeIcon icon={faCheck} size="xs" /> : idx + 1}
              </div>
              <span>{label}</span>
            </div>
          );
        })}
      </div>

      {/* Error Banner */}
      {error && (
        <div className="si-error">
          <FontAwesomeIcon icon={faExclamationCircle} />
          <span>{error}</span>
          <button onClick={() => setError(null)}><FontAwesomeIcon icon={faTimes} /></button>
        </div>
      )}

      {/* Step: Templates */}
      {step === 'templates' && (
        <div className="si-templates">
          <div className="si-section-header">
            <h3>1. Download a Template</h3>
            <p>Start with a template to ensure your data is formatted correctly</p>
          </div>

          <div className="si-template-grid">
            {templates.map((template) => (
              <a
                key={template.id}
                href={template.download_url}
                className="si-template-card"
                download
              >
                <div className="si-template-icon">
                  <FontAwesomeIcon icon={faFileAlt} size="2x" />
                </div>
                <div className="si-template-info">
                  <h4>{template.name}</h4>
                  <p>{template.description}</p>
                  <span className="si-template-fields">{template.fields} fields</span>
                </div>
                <FontAwesomeIcon icon={faDownload} className="si-download-icon" />
              </a>
            ))}
          </div>

          <div className="si-tips">
            <h4><FontAwesomeIcon icon={faQuestionCircle} /> Import Tips</h4>
            <ul>
              {tips.map((tip, i) => (
                <li key={i}>{tip}</li>
              ))}
            </ul>
          </div>

          {/* Available Fields Reference */}
          <div className="si-fields-reference">
            <h4>Available Fields</h4>
            {['required', 'profile', 'location', 'business', 'preferences', 'dates'].map((category) => {
              const categoryFields = fields.filter(f => f.category === category);
              if (categoryFields.length === 0) return null;

              return (
                <div key={category} className="si-field-category">
                  <button 
                    className="si-category-header"
                    onClick={() => toggleCategory(category)}
                  >
                    {expandedCategories.has(category) ? <FontAwesomeIcon icon={faChevronDown} /> : <FontAwesomeIcon icon={faChevronRight} />}
                    <span className="si-category-name">{category.charAt(0).toUpperCase() + category.slice(1)}</span>
                    <span className="si-category-count">{categoryFields.length}</span>
                  </button>
                  {expandedCategories.has(category) && (
                    <div className="si-category-fields">
                      {categoryFields.map((field) => (
                        <div key={field.key} className="si-field-item">
                          <code>{field.key}</code>
                          <span className="si-field-label">{field.label}</span>
                          {field.required && <span className="si-required-badge">Required</span>}
                          {field.example && <span className="si-field-example">e.g., {field.example}</span>}
                        </div>
                      ))}
                    </div>
                  )}
                </div>
              );
            })}
          </div>

          <div className="si-actions">
            <button className="si-btn si-btn-primary" onClick={() => setStep('upload')}>
              I Have My File Ready <FontAwesomeIcon icon={faChevronRight} />
            </button>
          </div>
        </div>
      )}

      {/* Step: Upload */}
      {step === 'upload' && (
        <div className="si-upload">
          <div className="si-section-header">
            <h3>2. Upload Your File</h3>
            <p>Select a CSV file to import</p>
          </div>

          <div
            className={`si-dropzone ${loading ? 'loading' : ''}`}
            onDragOver={(e) => e.preventDefault()}
            onDrop={handleDrop}
          >
            {loading ? (
              <div className="si-loading">
                <div className="si-spinner"></div>
                <p>Processing file...</p>
              </div>
            ) : (
              <>
                <FontAwesomeIcon icon={faUpload} size="3x" />
                <h4>Drag & drop your CSV file here</h4>
                <p>or</p>
                <label className="si-file-input">
                  <input
                    type="file"
                    accept=".csv,text/csv"
                    onChange={(e) => e.target.files?.[0] && handleFileSelect(e.target.files[0])}
                  />
                  <span>Browse Files</span>
                </label>
              </>
            )}
          </div>

          <div className="si-actions">
            <button className="si-btn" onClick={() => setStep('templates')}>
              Back to Templates
            </button>
          </div>
        </div>
      )}

      {/* Step: Mapping */}
      {step === 'mapping' && (
        <div className="si-mapping">
          <div className="si-section-header">
            <h3>3. Map Your Columns</h3>
            <p>Match your file columns to subscriber fields. We've suggested mappings based on your headers.</p>
          </div>

          {file && (
            <div className="si-file-info">
              <FontAwesomeIcon icon={faFileAlt} />
              <span>{file.name}</span>
              <span className="si-file-size">({(file.size / 1024).toFixed(1)} KB)</span>
              <button onClick={() => { setFile(null); setStep('upload'); }}>Change File</button>
            </div>
          )}

          <div className="si-mapping-table-wrapper">
            <table className="si-mapping-table">
              <thead>
                <tr>
                  <th>Your Column</th>
                  <th>Maps To</th>
                  <th>Preview</th>
                </tr>
              </thead>
              <tbody>
                {mapping.map((col, idx) => (
                  <tr key={idx} className={col.suggested_field === 'email' ? 'si-email-row' : ''}>
                    <td>
                      <strong>{col.original_header}</strong>
                      {col.confidence === 'high' && <FontAwesomeIcon icon={faCheck} className="si-confidence-high" />}
                    </td>
                    <td>
                      <select
                        value={getFinalMapping(idx)}
                        onChange={(e) => updateMapping(idx, e.target.value)}
                      >
                        <option value="">-- Ignore this column --</option>
                        <optgroup label="Required">
                          <option value="email">Email Address</option>
                        </optgroup>
                        <optgroup label="Profile">
                          <option value="first_name">First Name</option>
                          <option value="last_name">Last Name</option>
                          <option value="phone">Phone Number</option>
                        </optgroup>
                        <optgroup label="Location">
                          <option value="city">City</option>
                          <option value="state">State/Province</option>
                          <option value="country">Country</option>
                          <option value="postal_code">Postal/ZIP Code</option>
                          <option value="timezone">Timezone</option>
                        </optgroup>
                        <optgroup label="Business">
                          <option value="company">Company</option>
                          <option value="job_title">Job Title</option>
                          <option value="industry">Industry</option>
                        </optgroup>
                        <optgroup label="Preferences">
                          <option value="language">Language</option>
                          <option value="source">Source</option>
                          <option value="tags">Tags</option>
                        </optgroup>
                        <optgroup label="Dates">
                          <option value="birthdate">Birth Date</option>
                          <option value="subscribed_at">Subscribe Date</option>
                        </optgroup>
                        <optgroup label="Custom">
                          <option value={`custom_${col.original_header.toLowerCase().replace(/\s+/g, '_')}`}>
                            Custom: {col.original_header}
                          </option>
                        </optgroup>
                      </select>
                    </td>
                    <td className="si-preview-cell">
                      {previewRows[0]?.[idx] || <em>empty</em>}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>

          {/* Data Preview */}
          {previewRows.length > 1 && (
            <div className="si-data-preview">
              <h4>Data Preview (first {previewRows.length} rows)</h4>
              <div className="si-preview-scroll">
                <table>
                  <thead>
                    <tr>
                      {headers.map((h, i) => (
                        <th key={i}>{h}</th>
                      ))}
                    </tr>
                  </thead>
                  <tbody>
                    {previewRows.map((row, ri) => (
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

          <div className="si-actions">
            <button className="si-btn" onClick={() => setStep('upload')}>
              Back
            </button>
            <button 
              className="si-btn si-btn-primary" 
              onClick={startImport}
              disabled={!mapping.some(m => (customMapping[m.column_index] ?? m.suggested_field) === 'email')}
            >
              Start Import
            </button>
          </div>
        </div>
      )}

      {/* Step: Importing */}
      {step === 'importing' && importJob && (
        <div className="si-importing">
          <div className="si-progress-container">
            <div className="si-progress-icon">
              <div className="si-spinner large"></div>
            </div>
            <h3>Importing Subscribers...</h3>
            <p>Please wait while we process your file</p>
            {importJob.total_rows && importJob.processed_rows && (
              <div className="si-progress-bar-container">
                <div 
                  className="si-progress-bar"
                  style={{ width: `${(importJob.processed_rows / importJob.total_rows) * 100}%` }}
                ></div>
              </div>
            )}
            {importJob.processed_rows && (
              <p className="si-progress-text">
                Processed {importJob.processed_rows.toLocaleString()} rows...
              </p>
            )}
          </div>
        </div>
      )}

      {/* Step: Complete */}
      {step === 'complete' && importJob && (
        <div className="si-complete">
          <div className="si-complete-icon">
            <FontAwesomeIcon icon={faCheck} size="3x" />
          </div>
          <h3>Import Complete!</h3>
          
          <div className="si-stats-grid">
            <div className="si-stat">
              <span className="si-stat-value">{importJob.total_rows?.toLocaleString() || 0}</span>
              <span className="si-stat-label">Total Rows</span>
            </div>
            <div className="si-stat success">
              <span className="si-stat-value">{importJob.imported_count?.toLocaleString() || 0}</span>
              <span className="si-stat-label">Imported</span>
            </div>
            <div className="si-stat warning">
              <span className="si-stat-value">{importJob.skipped_count?.toLocaleString() || 0}</span>
              <span className="si-stat-label">Skipped</span>
            </div>
            <div className="si-stat error">
              <span className="si-stat-value">{importJob.error_count?.toLocaleString() || 0}</span>
              <span className="si-stat-label">Errors</span>
            </div>
          </div>

          {(importJob.skipped_count || 0) > 0 && (
            <p className="si-skip-note">
              Skipped rows may include duplicates, invalid emails, or suppressed addresses.
            </p>
          )}

          <div className="si-actions">
            <button className="si-btn si-btn-primary" onClick={onCancel}>
              Done
            </button>
            <button className="si-btn" onClick={() => { setStep('templates'); setFile(null); }}>
              Import More
            </button>
          </div>
        </div>
      )}

      <style>{`
        .subscriber-import {
          background: white;
          border-radius: 12px;
          max-width: 900px;
          margin: 0 auto;
        }

        .si-header {
          display: flex;
          justify-content: space-between;
          align-items: center;
          padding: 20px 24px;
          border-bottom: 1px solid #e5e7eb;
        }

        .si-header h2 { margin: 0; font-size: 20px; }
        .si-header p { margin: 4px 0 0; color: #6b7280; font-size: 14px; }
        .si-close { background: none; border: none; cursor: pointer; color: #9ca3af; }

        .si-steps {
          display: flex;
          justify-content: center;
          gap: 24px;
          padding: 20px;
          background: #f9fafb;
        }

        .si-step {
          display: flex;
          align-items: center;
          gap: 8px;
          color: #9ca3af;
          font-size: 14px;
        }

        .si-step.current { color: #3b82f6; font-weight: 500; }
        .si-step.complete { color: #10b981; }

        .si-step-indicator {
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

        .si-step.current .si-step-indicator { background: #3b82f6; color: white; }
        .si-step.complete .si-step-indicator { background: #10b981; color: white; }

        .si-error {
          display: flex;
          align-items: center;
          gap: 10px;
          padding: 12px 24px;
          background: #fef2f2;
          border-bottom: 1px solid #fecaca;
          color: #dc2626;
          font-size: 14px;
        }

        .si-error button {
          margin-left: auto;
          background: none;
          border: none;
          cursor: pointer;
          color: #dc2626;
        }

        .si-section-header {
          padding: 24px 24px 0;
        }

        .si-section-header h3 { margin: 0 0 4px; font-size: 18px; }
        .si-section-header p { margin: 0; color: #6b7280; }

        /* Templates */
        .si-templates { padding-bottom: 24px; }

        .si-template-grid {
          display: grid;
          grid-template-columns: repeat(auto-fit, minmax(250px, 1fr));
          gap: 16px;
          padding: 20px 24px;
        }

        .si-template-card {
          display: flex;
          align-items: center;
          gap: 16px;
          padding: 16px;
          border: 1px solid #e5e7eb;
          border-radius: 8px;
          text-decoration: none;
          color: inherit;
          transition: all 0.2s;
        }

        .si-template-card:hover {
          border-color: #3b82f6;
          background: #f0f9ff;
        }

        .si-template-icon { color: #3b82f6; }
        .si-template-info h4 { margin: 0 0 4px; font-size: 15px; }
        .si-template-info p { margin: 0; font-size: 13px; color: #6b7280; }
        .si-template-fields { font-size: 12px; color: #9ca3af; }
        .si-download-icon { margin-left: auto; color: #9ca3af; }

        .si-tips {
          margin: 0 24px;
          padding: 16px;
          background: #fffbeb;
          border-radius: 8px;
        }

        .si-tips h4 {
          display: flex;
          align-items: center;
          gap: 8px;
          margin: 0 0 12px;
          font-size: 14px;
          color: #92400e;
        }

        .si-tips ul {
          margin: 0;
          padding-left: 20px;
          font-size: 13px;
          color: #78716c;
        }

        .si-tips li { margin-bottom: 4px; }

        /* Fields Reference */
        .si-fields-reference {
          margin: 20px 24px;
          border: 1px solid #e5e7eb;
          border-radius: 8px;
          overflow: hidden;
        }

        .si-fields-reference h4 {
          margin: 0;
          padding: 12px 16px;
          background: #f9fafb;
          font-size: 14px;
          border-bottom: 1px solid #e5e7eb;
        }

        .si-field-category { border-bottom: 1px solid #e5e7eb; }
        .si-field-category:last-child { border-bottom: none; }

        .si-category-header {
          display: flex;
          align-items: center;
          gap: 8px;
          width: 100%;
          padding: 12px 16px;
          background: none;
          border: none;
          cursor: pointer;
          font-size: 14px;
          text-align: left;
        }

        .si-category-header:hover { background: #f9fafb; }
        .si-category-name { font-weight: 500; }
        .si-category-count { margin-left: auto; color: #9ca3af; font-size: 12px; }

        .si-category-fields { padding: 8px 16px 16px 40px; }

        .si-field-item {
          display: flex;
          align-items: center;
          gap: 12px;
          padding: 6px 0;
          font-size: 13px;
        }

        .si-field-item code {
          background: #f3f4f6;
          padding: 2px 6px;
          border-radius: 4px;
          font-size: 12px;
        }

        .si-field-label { color: #374151; }
        .si-required-badge {
          background: #fee2e2;
          color: #dc2626;
          padding: 2px 6px;
          border-radius: 4px;
          font-size: 11px;
        }
        .si-field-example { color: #9ca3af; font-size: 12px; }

        /* Upload */
        .si-upload { padding: 24px; }

        .si-dropzone {
          display: flex;
          flex-direction: column;
          align-items: center;
          justify-content: center;
          padding: 48px;
          border: 2px dashed #d1d5db;
          border-radius: 12px;
          background: #f9fafb;
          margin-top: 20px;
          text-align: center;
        }

        .si-dropzone.loading { pointer-events: none; opacity: 0.7; }

        .si-dropzone h4 { margin: 16px 0 8px; }
        .si-dropzone p { margin: 0 0 16px; color: #6b7280; }

        .si-file-input input { display: none; }
        .si-file-input span {
          display: inline-block;
          padding: 10px 20px;
          background: #3b82f6;
          color: white;
          border-radius: 6px;
          cursor: pointer;
          font-weight: 500;
        }

        .si-file-input span:hover { background: #2563eb; }

        /* Mapping */
        .si-mapping { padding: 0 24px 24px; }

        .si-file-info {
          display: flex;
          align-items: center;
          gap: 8px;
          padding: 12px 16px;
          background: #f0f9ff;
          border-radius: 8px;
          margin-top: 16px;
          font-size: 14px;
        }

        .si-file-size { color: #6b7280; }
        .si-file-info button {
          margin-left: auto;
          background: none;
          border: none;
          color: #3b82f6;
          cursor: pointer;
          font-size: 13px;
        }

        .si-mapping-table-wrapper {
          margin-top: 20px;
          border: 1px solid #e5e7eb;
          border-radius: 8px;
          overflow: hidden;
        }

        .si-mapping-table {
          width: 100%;
          border-collapse: collapse;
        }

        .si-mapping-table th,
        .si-mapping-table td {
          padding: 12px;
          text-align: left;
          border-bottom: 1px solid #e5e7eb;
        }

        .si-mapping-table th {
          background: #f9fafb;
          font-size: 12px;
          font-weight: 600;
          color: #6b7280;
          text-transform: uppercase;
        }

        .si-mapping-table select {
          width: 100%;
          padding: 8px;
          border: 1px solid #d1d5db;
          border-radius: 6px;
          font-size: 14px;
        }

        .si-email-row { background: #f0fdf4; }

        .si-confidence-high { color: #10b981; margin-left: 8px; }

        .si-preview-cell {
          max-width: 150px;
          overflow: hidden;
          text-overflow: ellipsis;
          white-space: nowrap;
          color: #6b7280;
          font-size: 13px;
        }

        .si-data-preview {
          margin-top: 20px;
        }

        .si-data-preview h4 { margin: 0 0 12px; font-size: 14px; }

        .si-preview-scroll {
          overflow-x: auto;
          border: 1px solid #e5e7eb;
          border-radius: 8px;
        }

        .si-preview-scroll table {
          width: 100%;
          border-collapse: collapse;
          font-size: 13px;
        }

        .si-preview-scroll th,
        .si-preview-scroll td {
          padding: 8px 12px;
          border-bottom: 1px solid #e5e7eb;
          white-space: nowrap;
        }

        .si-preview-scroll th { background: #f9fafb; font-weight: 500; }

        /* Importing & Complete */
        .si-importing, .si-complete {
          padding: 48px 24px;
          text-align: center;
        }

        .si-progress-container h3 { margin: 20px 0 8px; }
        .si-progress-container p { color: #6b7280; margin: 0; }

        .si-progress-bar-container {
          width: 100%;
          max-width: 400px;
          height: 8px;
          background: #e5e7eb;
          border-radius: 4px;
          margin: 24px auto;
          overflow: hidden;
        }

        .si-progress-bar {
          height: 100%;
          background: #3b82f6;
          transition: width 0.3s;
        }

        .si-progress-text { color: #6b7280; font-size: 14px; }

        .si-complete-icon {
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

        .si-stats-grid {
          display: grid;
          grid-template-columns: repeat(4, 1fr);
          gap: 16px;
          margin: 24px 0;
          max-width: 500px;
          margin-left: auto;
          margin-right: auto;
        }

        .si-stat {
          padding: 16px;
          background: #f9fafb;
          border-radius: 8px;
        }

        .si-stat-value { display: block; font-size: 24px; font-weight: 700; }
        .si-stat-label { font-size: 12px; color: #6b7280; }
        .si-stat.success .si-stat-value { color: #10b981; }
        .si-stat.warning .si-stat-value { color: #f59e0b; }
        .si-stat.error .si-stat-value { color: #ef4444; }

        .si-skip-note { color: #6b7280; font-size: 13px; }

        /* Actions */
        .si-actions {
          display: flex;
          justify-content: flex-end;
          gap: 12px;
          padding: 20px 24px;
          border-top: 1px solid #e5e7eb;
          margin-top: 20px;
        }

        .si-btn {
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
        }

        .si-btn:hover { background: #f9fafb; }
        .si-btn:disabled { opacity: 0.5; cursor: not-allowed; }

        .si-btn-primary {
          background: #3b82f6;
          border-color: #3b82f6;
          color: white;
        }

        .si-btn-primary:hover { background: #2563eb; }

        /* Loading */
        .si-loading { text-align: center; }

        .si-spinner {
          width: 32px;
          height: 32px;
          border: 3px solid #e5e7eb;
          border-top-color: #3b82f6;
          border-radius: 50%;
          animation: spin 0.8s linear infinite;
          margin: 0 auto;
        }

        .si-spinner.large { width: 48px; height: 48px; }

        @keyframes spin {
          to { transform: rotate(360deg); }
        }
      `}</style>
    </div>
  );
};

export default SubscriberImport;
