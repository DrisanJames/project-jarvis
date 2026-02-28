import React from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faCalendar, faChevronDown } from '@fortawesome/free-solid-svg-icons';
import { useDateFilter, DateRangeType } from '../../context/DateFilterContext';

const dateRangeOptions: { type: DateRangeType; label: string; description: string; group: string }[] = [
  { type: 'today',     label: 'Today',          description: 'Current day only',           group: 'Quick' },
  { type: 'last24h',   label: 'Last 24 Hours',  description: 'Yesterday through today',    group: 'Quick' },
  { type: 'last7',     label: '7 Days',         description: 'Rolling 7-day window',       group: 'Rolling' },
  { type: 'last14',    label: '14 Days',        description: 'Rolling 14-day window',      group: 'Rolling' },
  { type: 'last30',    label: '30 Days',        description: 'Rolling 30-day window',      group: 'Rolling' },
  { type: 'last60',    label: '60 Days',        description: 'Rolling 60-day window',      group: 'Rolling' },
  { type: 'last90',    label: '90 Days',        description: 'Rolling 90-day window',      group: 'Rolling' },
  { type: 'mtd',       label: 'Month to Date',  description: 'From 1st of current month',  group: 'Period' },
  { type: 'lastMonth', label: 'Last Month',     description: 'Previous complete month',     group: 'Period' },
  { type: 'ytd',       label: 'Year to Date',   description: 'From Jan 1 of current year', group: 'Period' },
];

export const DateFilter: React.FC = () => {
  const { dateRange, setDateRangeType, setCustomRange } = useDateFilter();
  const [isOpen, setIsOpen] = React.useState(false);
  const [showCustom, setShowCustom] = React.useState(false);
  const [customStart, setCustomStart] = React.useState('');
  const [customEnd, setCustomEnd] = React.useState('');
  const dropdownRef = React.useRef<HTMLDivElement>(null);

  React.useEffect(() => {
    const handleClickOutside = (event: MouseEvent) => {
      if (dropdownRef.current && !dropdownRef.current.contains(event.target as Node)) {
        setIsOpen(false);
        setShowCustom(false);
      }
    };
    document.addEventListener('mousedown', handleClickOutside);
    return () => document.removeEventListener('mousedown', handleClickOutside);
  }, []);

  const formatDateDisplay = (dateStr: string): string => {
    const date = new Date(dateStr + 'T00:00:00');
    return date.toLocaleDateString('en-US', { month: 'short', day: 'numeric' });
  };

  const handleCustomApply = () => {
    if (customStart && customEnd) {
      setCustomRange(customStart, customEnd);
      setIsOpen(false);
      setShowCustom(false);
    }
  };

  // Group options
  const groups = ['Quick', 'Rolling', 'Period'];

  return (
    <div className="date-filter" ref={dropdownRef}>
      <button
        className="date-filter-button"
        onClick={() => setIsOpen(!isOpen)}
        aria-expanded={isOpen}
      >
        <FontAwesomeIcon icon={faCalendar} className="fa-icon-sm" />
        <span className="date-filter-label">{dateRange.label}</span>
        <span className="date-filter-range">
          {formatDateDisplay(dateRange.startDate)} - {formatDateDisplay(dateRange.endDate)}
        </span>
        <FontAwesomeIcon icon={faChevronDown} className={`fa-icon-sm chevron ${isOpen ? 'open' : ''}`} />
      </button>

      {isOpen && (
        <div className="date-filter-dropdown">
          {!showCustom ? (
            <>
              {groups.map((group) => (
                <div key={group}>
                  <div className="date-filter-group-label">{group}</div>
                  {dateRangeOptions.filter(o => o.group === group).map((option) => (
                    <button
                      key={option.type}
                      className={`date-filter-option ${dateRange.type === option.type ? 'active' : ''}`}
                      onClick={() => {
                        setDateRangeType(option.type);
                        setIsOpen(false);
                      }}
                    >
                      <div className="option-content">
                        <span className="option-label">{option.label}</span>
                        <span className="option-description">{option.description}</span>
                      </div>
                      {dateRange.type === option.type && (
                        <span className="option-check">✓</span>
                      )}
                    </button>
                  ))}
                </div>
              ))}
              <div className="date-filter-divider" />
              <button
                className={`date-filter-option ${dateRange.type === 'custom' ? 'active' : ''}`}
                onClick={() => setShowCustom(true)}
              >
                <div className="option-content">
                  <span className="option-label">Custom Range</span>
                  <span className="option-description">Pick specific start & end dates</span>
                </div>
              </button>
            </>
          ) : (
            <div className="date-filter-custom">
              <div className="custom-header">Custom Date Range</div>
              <div className="custom-inputs">
                <label>
                  <span className="custom-label">Start</span>
                  <input
                    type="date"
                    value={customStart}
                    onChange={(e) => setCustomStart(e.target.value)}
                    className="custom-date-input"
                  />
                </label>
                <label>
                  <span className="custom-label">End</span>
                  <input
                    type="date"
                    value={customEnd}
                    onChange={(e) => setCustomEnd(e.target.value)}
                    className="custom-date-input"
                  />
                </label>
              </div>
              <div className="custom-actions">
                <button className="custom-btn cancel" onClick={() => setShowCustom(false)}>Cancel</button>
                <button
                  className="custom-btn apply"
                  onClick={handleCustomApply}
                  disabled={!customStart || !customEnd}
                >
                  Apply
                </button>
              </div>
            </div>
          )}
        </div>
      )}

      <style>{`
        .date-filter {
          position: relative;
        }
        .date-filter-button {
          display: flex;
          align-items: center;
          gap: 0.5rem;
          padding: 0.5rem 0.75rem;
          background: var(--bg-tertiary, #2a2a3e);
          border: 1px solid var(--border-color, #333);
          border-radius: 6px;
          color: var(--text-primary);
          font-size: 0.8125rem;
          cursor: pointer;
          transition: all 0.2s;
        }
        .date-filter-button:hover {
          background: var(--bg-secondary, #1e1e2e);
          border-color: var(--accent-blue, #3b82f6);
        }
        .date-filter-label {
          font-weight: 500;
        }
        .date-filter-range {
          color: var(--text-muted);
          font-size: 0.75rem;
        }
        .chevron {
          transition: transform 0.2s;
        }
        .chevron.open {
          transform: rotate(180deg);
        }
        .date-filter-dropdown {
          position: absolute;
          top: calc(100% + 4px);
          right: 0;
          min-width: 260px;
          max-height: 460px;
          overflow-y: auto;
          background: var(--bg-secondary, #1e1e2e);
          border: 1px solid var(--border-color, #333);
          border-radius: 8px;
          box-shadow: 0 4px 12px rgba(0, 0, 0, 0.3);
          z-index: 1000;
        }
        .date-filter-group-label {
          padding: 0.5rem 1rem 0.25rem;
          font-size: 0.65rem;
          font-weight: 700;
          letter-spacing: 0.08em;
          text-transform: uppercase;
          color: var(--text-muted);
        }
        .date-filter-divider {
          height: 1px;
          background: var(--border-color, #333);
          margin: 0.25rem 0;
        }
        .date-filter-option {
          display: flex;
          align-items: center;
          justify-content: space-between;
          width: 100%;
          padding: 0.6rem 1rem;
          background: transparent;
          border: none;
          color: var(--text-primary);
          cursor: pointer;
          transition: background 0.15s;
          text-align: left;
        }
        .date-filter-option:hover {
          background: var(--bg-tertiary, #2a2a3e);
        }
        .date-filter-option.active {
          background: rgba(59, 130, 246, 0.15);
        }
        .option-content {
          display: flex;
          flex-direction: column;
          gap: 0.125rem;
        }
        .option-label {
          font-size: 0.8125rem;
          font-weight: 500;
        }
        .option-description {
          font-size: 0.6875rem;
          color: var(--text-muted);
        }
        .option-check {
          color: var(--accent-blue, #3b82f6);
          font-weight: 600;
        }
        .date-filter-custom {
          padding: 0.75rem 1rem;
        }
        .custom-header {
          font-size: 0.8125rem;
          font-weight: 600;
          margin-bottom: 0.75rem;
          color: var(--text-primary);
        }
        .custom-inputs {
          display: flex;
          flex-direction: column;
          gap: 0.5rem;
        }
        .custom-label {
          display: block;
          font-size: 0.6875rem;
          color: var(--text-muted);
          margin-bottom: 0.25rem;
          font-weight: 500;
        }
        .custom-date-input {
          width: 100%;
          padding: 0.4rem 0.5rem;
          background: var(--bg-tertiary, #2a2a3e);
          border: 1px solid var(--border-color, #333);
          border-radius: 4px;
          color: var(--text-primary);
          font-size: 0.8125rem;
          font-family: monospace;
        }
        .custom-date-input:focus {
          outline: none;
          border-color: var(--accent-blue, #3b82f6);
        }
        .custom-actions {
          display: flex;
          gap: 0.5rem;
          margin-top: 0.75rem;
          justify-content: flex-end;
        }
        .custom-btn {
          padding: 0.4rem 0.75rem;
          border-radius: 4px;
          font-size: 0.75rem;
          font-weight: 500;
          cursor: pointer;
          border: none;
          transition: all 0.15s;
        }
        .custom-btn.cancel {
          background: var(--bg-tertiary, #2a2a3e);
          color: var(--text-muted);
        }
        .custom-btn.cancel:hover {
          background: var(--border-color, #333);
        }
        .custom-btn.apply {
          background: var(--accent-blue, #3b82f6);
          color: white;
        }
        .custom-btn.apply:hover {
          opacity: 0.9;
        }
        .custom-btn.apply:disabled {
          opacity: 0.4;
          cursor: not-allowed;
        }
      `}</style>
    </div>
  );
};
