import React, { useState, useMemo } from 'react';
import Select, { components, MultiValue, StylesConfig } from 'react-select';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { 
  faStar, 
  faLock,
  faExclamationTriangle,
  faCheckCircle,
  faShieldAlt,
  faUsers,
  faBan
} from '@fortawesome/free-solid-svg-icons';
import './SearchableMultiSelect.css';

// =============================================================================
// TYPES
// =============================================================================

export interface SelectOption {
  id: string;
  name: string;
  count?: number;
  category?: string;
  isFavorite?: boolean;
  isRecent?: boolean;
  isLocked?: boolean;  // Can't be deselected (e.g., Global Suppression)
  isRequired?: boolean;
  lastSyncAt?: string;
  description?: string;
  type?: 'segment' | 'suppression' | 'list';
  icon?: string;
}

interface SearchableMultiSelectProps {
  options: SelectOption[];
  selected: string[];
  onChange: (selected: string[]) => void;
  placeholder?: string;
  label?: string;
  type?: 'segment' | 'suppression';
  showEstimate?: boolean;
  emptyMessage?: string;
  maxHeight?: number;
  allowCreate?: boolean;
  onCreateClick?: () => void;
}

// =============================================================================
// CUSTOM STYLES
// =============================================================================

const customStyles: StylesConfig<any, true> = {
  control: (base, state) => ({
    ...base,
    minHeight: '44px',
    borderRadius: '10px',
    borderColor: state.isFocused ? '#667eea' : '#e2e8f0',
    boxShadow: state.isFocused ? '0 0 0 3px rgba(102, 126, 234, 0.15)' : 'none',
    '&:hover': {
      borderColor: '#667eea',
    },
    backgroundColor: '#fff',
  }),
  menu: (base) => ({
    ...base,
    borderRadius: '12px',
    boxShadow: '0 10px 40px rgba(0, 0, 0, 0.15)',
    border: '1px solid #e2e8f0',
    overflow: 'hidden',
    zIndex: 9999,
  }),
  menuList: (base) => ({
    ...base,
    padding: '8px',
    maxHeight: '350px',
  }),
  option: (base, state) => ({
    ...base,
    borderRadius: '8px',
    padding: '10px 12px',
    marginBottom: '2px',
    backgroundColor: state.isSelected 
      ? '#667eea' 
      : state.isFocused 
        ? '#f0f4ff' 
        : 'transparent',
    color: state.isSelected ? '#fff' : '#2d3748',
    cursor: 'pointer',
    '&:active': {
      backgroundColor: state.isSelected ? '#667eea' : '#e2e8f0',
    },
  }),
  multiValue: (base, state) => ({
    ...base,
    backgroundColor: (state.data as any)?.isLocked ? '#fed7d7' : '#e2e8f0',
    borderRadius: '6px',
    padding: '2px 4px',
  }),
  multiValueLabel: (base, state) => ({
    ...base,
    color: (state.data as any)?.isLocked ? '#c53030' : '#2d3748',
    fontWeight: 500,
    fontSize: '13px',
  }),
  multiValueRemove: (base, state) => ({
    ...base,
    display: (state.data as any)?.isLocked ? 'none' : 'flex',
    color: '#718096',
    '&:hover': {
      backgroundColor: '#c53030',
      color: '#fff',
    },
  }),
  placeholder: (base) => ({
    ...base,
    color: '#a0aec0',
  }),
  input: (base) => ({
    ...base,
    color: '#2d3748',
  }),
  groupHeading: (base) => ({
    ...base,
    fontSize: '11px',
    fontWeight: 700,
    textTransform: 'uppercase',
    color: '#718096',
    padding: '8px 12px 4px',
    letterSpacing: '0.05em',
  }),
};

// =============================================================================
// CUSTOM COMPONENTS
// =============================================================================

const CustomOption = (props: any) => {
  const { data, isSelected } = props;
  const option = data as SelectOption;
  
  return (
    <components.Option {...props}>
      <div className="sms-option">
        <div className="sms-option-left">
          {option.isLocked && (
            <FontAwesomeIcon icon={faLock} className="sms-option-lock" />
          )}
          {option.isFavorite && !option.isLocked && (
            <FontAwesomeIcon icon={faStar} className="sms-option-star" />
          )}
          <span className="sms-option-name">{option.name}</span>
          {option.isRequired && (
            <span className="sms-option-required">Required</span>
          )}
        </div>
        <div className="sms-option-right">
          {option.count !== undefined && (
            <span className="sms-option-count">
              {option.count.toLocaleString()}
            </span>
          )}
          {isSelected && (
            <FontAwesomeIcon icon={faCheckCircle} className="sms-option-check" />
          )}
        </div>
      </div>
    </components.Option>
  );
};

const CustomMultiValueLabel = (props: any) => {
  const option = props.data as SelectOption;
  return (
    <components.MultiValueLabel {...props}>
      <span className="sms-tag-content">
        {option.isLocked && (
          <FontAwesomeIcon icon={faLock} className="sms-tag-lock" />
        )}
        {option.name}
        {option.count !== undefined && (
          <span className="sms-tag-count">({option.count.toLocaleString()})</span>
        )}
      </span>
    </components.MultiValueLabel>
  );
};

const CustomMenuList = (props: any) => {
  return (
    <components.MenuList {...props}>
      {props.children}
    </components.MenuList>
  );
};

// =============================================================================
// MAIN COMPONENT
// =============================================================================

export const SearchableMultiSelect: React.FC<SearchableMultiSelectProps> = ({
  options,
  selected,
  onChange,
  placeholder = 'Search and select...',
  label,
  type = 'segment',
  showEstimate = true,
  emptyMessage = 'No options available',
  allowCreate = false,
  onCreateClick,
}) => {
  const [inputValue, setInputValue] = useState('');

  // Group options by category
  const groupedOptions = useMemo(() => {
    if (options.length === 0) return [];

    const favorites = options.filter(o => o.isFavorite);
    const locked = options.filter(o => o.isLocked);
    const recent = options.filter(o => o.isRecent && !o.isFavorite && !o.isLocked);
    const rest = options.filter(o => !o.isFavorite && !o.isRecent && !o.isLocked);

    const groups: any[] = [];

    if (locked.length > 0) {
      groups.push({
        label: type === 'suppression' ? 'Required Suppressions' : 'Required',
        options: locked.map(o => ({ ...o, value: o.id, label: o.name })),
      });
    }

    if (favorites.length > 0) {
      groups.push({
        label: 'Favorites',
        options: favorites.map(o => ({ ...o, value: o.id, label: o.name })),
      });
    }

    if (recent.length > 0) {
      groups.push({
        label: 'Recently Used',
        options: recent.map(o => ({ ...o, value: o.id, label: o.name })),
      });
    }

    // Group rest by category
    const byCategory = rest.reduce((acc, o) => {
      const cat = o.category || 'Other';
      if (!acc[cat]) acc[cat] = [];
      acc[cat].push(o);
      return acc;
    }, {} as Record<string, SelectOption[]>);

    Object.entries(byCategory).sort(([a], [b]) => a.localeCompare(b)).forEach(([cat, items]) => {
      groups.push({
        label: cat,
        options: items.map(o => ({ ...o, value: o.id, label: o.name })),
      });
    });

    return groups;
  }, [options, type]);

  // Convert selected IDs to option objects
  const selectedOptions = useMemo(() => {
    return options
      .filter(o => selected.includes(o.id))
      .map(o => ({ ...o, value: o.id, label: o.name }));
  }, [options, selected]);

  // Calculate estimated reach
  const estimatedReach = useMemo(() => {
    return options
      .filter(o => selected.includes(o.id))
      .reduce((sum, o) => sum + (o.count || 0), 0);
  }, [options, selected]);

  const handleChange = (newValue: MultiValue<any>) => {
    // Keep locked items that were already selected
    const lockedIds = options.filter(o => o.isLocked && selected.includes(o.id)).map(o => o.id);
    const newIds = newValue.map((v: any) => v.id);
    
    // Merge locked items back if they were removed
    const finalIds = [...new Set([...lockedIds, ...newIds])];
    onChange(finalIds);
  };

  const getIcon = () => {
    if (type === 'suppression') return faBan;
    return faUsers;
  };

  return (
    <div className="searchable-multi-select">
      {label && (
        <label className="sms-label">
          <FontAwesomeIcon icon={getIcon()} className="sms-label-icon" />
          {label}
          {allowCreate && onCreateClick && (
            <button className="sms-create-btn" onClick={onCreateClick}>
              + Create New
            </button>
          )}
        </label>
      )}
      
      {options.length === 0 ? (
        <div className="sms-empty">
          <FontAwesomeIcon icon={faExclamationTriangle} />
          <span>{emptyMessage}</span>
        </div>
      ) : (
        <>
          <Select
            isMulti
            options={groupedOptions}
            value={selectedOptions}
            onChange={handleChange}
            inputValue={inputValue}
            onInputChange={setInputValue}
            placeholder={placeholder}
            styles={customStyles}
            components={{
              Option: CustomOption,
              MultiValueLabel: CustomMultiValueLabel,
              MenuList: CustomMenuList,
            }}
            closeMenuOnSelect={false}
            hideSelectedOptions={false}
            isClearable={false}
            isSearchable
            filterOption={(option, input) => {
              if (!input) return true;
              const searchLower = input.toLowerCase();
              return (
                option.data.name?.toLowerCase().includes(searchLower) ||
                option.data.category?.toLowerCase().includes(searchLower)
              );
            }}
            noOptionsMessage={() => 'No matches found'}
          />
          
          {showEstimate && selected.length > 0 && (
            <div className="sms-estimate">
              <FontAwesomeIcon icon={type === 'suppression' ? faShieldAlt : faUsers} />
              <span>
                {type === 'suppression' 
                  ? `${estimatedReach.toLocaleString()} contacts will be suppressed`
                  : `Estimated reach: ${estimatedReach.toLocaleString()} subscribers`
                }
              </span>
            </div>
          )}
        </>
      )}
    </div>
  );
};

export default SearchableMultiSelect;
