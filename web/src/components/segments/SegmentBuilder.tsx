import React, { useState, useEffect, useCallback } from 'react';

// ==========================================
// TYPES
// ==========================================

export type FieldType = 'string' | 'number' | 'integer' | 'decimal' | 'boolean' | 'date' | 'datetime' | 'array' | 'tags' | 'event';
export type ConditionType = 'profile' | 'custom_field' | 'event' | 'computed' | 'tag';
export type LogicOperator = 'AND' | 'OR';
export type Operator = 
  // String
  'equals' | 'not_equals' | 'contains' | 'not_contains' | 'starts_with' | 'ends_with' | 'is_empty' | 'is_not_empty' | 'matches_regex' |
  // Numeric
  'gt' | 'gte' | 'lt' | 'lte' | 'between' | 'not_between' |
  // Date
  'date_equals' | 'date_before' | 'date_after' | 'date_between' | 'in_last_days' | 'in_next_days' | 'more_than_days_ago' | 'anniversary_month' | 'anniversary_day' |
  // Array
  'contains_any' | 'contains_all' | 'not_contains_any' | 'array_is_empty' | 'array_is_not_empty' |
  // Boolean
  'is_true' | 'is_false' |
  // NULL
  'is_null' | 'is_not_null' |
  // Event
  'event_count_gte' | 'event_count_lte' | 'event_count_between' | 'event_in_last_days' | 'event_not_in_last_days' | 'event_property_equals' | 'event_property_contains';

export interface ConditionBuilder {
  id: string;
  condition_type: ConditionType;
  field: string;
  field_type?: FieldType;
  operator: Operator;
  value?: string;
  value_secondary?: string;
  values_array?: string[];
  event_name?: string;
  event_time_window_days?: number;
  event_min_count?: number;
  event_max_count?: number;
  event_property_path?: string;
  event_sending_domain?: string;
}

export interface ConditionGroupBuilder {
  id: string;
  logic_operator: LogicOperator;
  is_negated: boolean;
  conditions: ConditionBuilder[];
  groups: ConditionGroupBuilder[];
}

export interface ContactField {
  field_key: string;
  field_label: string;
  field_type: FieldType;
  category: string;
  is_system: boolean;
}

export interface OperatorMeta {
  operator: Operator;
  label: string;
  description: string;
  applicable_types: FieldType[];
  requires_value: boolean;
  requires_secondary: boolean;
  requires_array: boolean;
}

export interface SegmentPreview {
  estimated_count: number;
  sample_subscribers: Array<{
    id: string;
    email: string;
    first_name?: string;
    last_name?: string;
    engagement_score: number;
  }>;
}

// ==========================================
// OPERATOR METADATA
// ==========================================

const OPERATORS: OperatorMeta[] = [
  // String operators
  { operator: 'equals', label: 'Equals', description: 'Exact match', applicable_types: ['string', 'number', 'integer'], requires_value: true, requires_secondary: false, requires_array: false },
  { operator: 'not_equals', label: 'Does not equal', description: 'Not an exact match', applicable_types: ['string', 'number', 'integer'], requires_value: true, requires_secondary: false, requires_array: false },
  { operator: 'contains', label: 'Contains', description: 'Contains the text', applicable_types: ['string'], requires_value: true, requires_secondary: false, requires_array: false },
  { operator: 'not_contains', label: 'Does not contain', description: 'Does not contain the text', applicable_types: ['string'], requires_value: true, requires_secondary: false, requires_array: false },
  { operator: 'starts_with', label: 'Starts with', description: 'Begins with the text', applicable_types: ['string'], requires_value: true, requires_secondary: false, requires_array: false },
  { operator: 'ends_with', label: 'Ends with', description: 'Ends with the text', applicable_types: ['string'], requires_value: true, requires_secondary: false, requires_array: false },
  { operator: 'is_empty', label: 'Is empty', description: 'Field is empty', applicable_types: ['string'], requires_value: false, requires_secondary: false, requires_array: false },
  { operator: 'is_not_empty', label: 'Is not empty', description: 'Field has a value', applicable_types: ['string'], requires_value: false, requires_secondary: false, requires_array: false },
  
  // Numeric operators
  { operator: 'gt', label: 'Greater than', description: 'Value is greater than', applicable_types: ['number', 'integer', 'decimal'], requires_value: true, requires_secondary: false, requires_array: false },
  { operator: 'gte', label: 'Greater than or equal', description: 'Value is greater than or equal', applicable_types: ['number', 'integer', 'decimal'], requires_value: true, requires_secondary: false, requires_array: false },
  { operator: 'lt', label: 'Less than', description: 'Value is less than', applicable_types: ['number', 'integer', 'decimal'], requires_value: true, requires_secondary: false, requires_array: false },
  { operator: 'lte', label: 'Less than or equal', description: 'Value is less than or equal', applicable_types: ['number', 'integer', 'decimal'], requires_value: true, requires_secondary: false, requires_array: false },
  { operator: 'between', label: 'Between', description: 'Value is between two numbers', applicable_types: ['number', 'integer', 'decimal'], requires_value: true, requires_secondary: true, requires_array: false },
  
  // Date operators
  { operator: 'date_equals', label: 'On date', description: 'Exactly on the date', applicable_types: ['date', 'datetime'], requires_value: true, requires_secondary: false, requires_array: false },
  { operator: 'date_before', label: 'Before date', description: 'Before the date', applicable_types: ['date', 'datetime'], requires_value: true, requires_secondary: false, requires_array: false },
  { operator: 'date_after', label: 'After date', description: 'After the date', applicable_types: ['date', 'datetime'], requires_value: true, requires_secondary: false, requires_array: false },
  { operator: 'in_last_days', label: 'In the last X days', description: 'Within the last N days', applicable_types: ['date', 'datetime'], requires_value: true, requires_secondary: false, requires_array: false },
  { operator: 'in_next_days', label: 'In the next X days', description: 'Within the next N days', applicable_types: ['date', 'datetime'], requires_value: true, requires_secondary: false, requires_array: false },
  { operator: 'more_than_days_ago', label: 'More than X days ago', description: 'More than N days in the past', applicable_types: ['date', 'datetime'], requires_value: true, requires_secondary: false, requires_array: false },
  { operator: 'anniversary_month', label: 'Anniversary month', description: 'Month matches (ignores year)', applicable_types: ['date', 'datetime'], requires_value: true, requires_secondary: false, requires_array: false },
  
  // Array operators
  { operator: 'contains_any', label: 'Contains any of', description: 'Contains at least one value', applicable_types: ['array', 'tags'], requires_value: false, requires_secondary: false, requires_array: true },
  { operator: 'contains_all', label: 'Contains all of', description: 'Contains all values', applicable_types: ['array', 'tags'], requires_value: false, requires_secondary: false, requires_array: true },
  { operator: 'not_contains_any', label: 'Does not contain any of', description: 'Contains none of the values', applicable_types: ['array', 'tags'], requires_value: false, requires_secondary: false, requires_array: true },
  { operator: 'array_is_empty', label: 'Array is empty', description: 'Array has no items', applicable_types: ['array', 'tags'], requires_value: false, requires_secondary: false, requires_array: false },
  
  // Boolean operators
  { operator: 'is_true', label: 'Is true', description: 'Boolean is true', applicable_types: ['boolean'], requires_value: false, requires_secondary: false, requires_array: false },
  { operator: 'is_false', label: 'Is false', description: 'Boolean is false', applicable_types: ['boolean'], requires_value: false, requires_secondary: false, requires_array: false },
  
  // NULL checks
  { operator: 'is_null', label: 'Is null', description: 'Value is null/missing', applicable_types: ['string', 'number', 'date', 'boolean'], requires_value: false, requires_secondary: false, requires_array: false },
  { operator: 'is_not_null', label: 'Is not null', description: 'Value exists', applicable_types: ['string', 'number', 'date', 'boolean'], requires_value: false, requires_secondary: false, requires_array: false },
  
  // Event operators
  { operator: 'event_count_gte', label: 'Occurred at least', description: 'Event occurred at least N times', applicable_types: ['event'], requires_value: true, requires_secondary: false, requires_array: false },
  { operator: 'event_count_lte', label: 'Occurred at most', description: 'Event occurred at most N times', applicable_types: ['event'], requires_value: true, requires_secondary: false, requires_array: false },
  { operator: 'event_in_last_days', label: 'Occurred in last X days', description: 'Event occurred recently', applicable_types: ['event'], requires_value: true, requires_secondary: false, requires_array: false },
  { operator: 'event_not_in_last_days', label: 'NOT occurred in last X days', description: 'Event did not occur recently', applicable_types: ['event'], requires_value: true, requires_secondary: false, requires_array: false },
];

// ==========================================
// DEFAULT FIELDS
// ==========================================

export const DEFAULT_FIELDS: ContactField[] = [
  // Contact Profile Fields
  { field_key: 'email', field_label: 'Email Address', field_type: 'string', category: 'profile', is_system: true },
  { field_key: 'first_name', field_label: 'First Name', field_type: 'string', category: 'profile', is_system: true },
  { field_key: 'last_name', field_label: 'Last Name', field_type: 'string', category: 'profile', is_system: true },
  { field_key: 'full_name', field_label: 'Full Name', field_type: 'string', category: 'profile', is_system: true },
  { field_key: 'phone', field_label: 'Phone Number', field_type: 'string', category: 'profile', is_system: true },
  { field_key: 'city', field_label: 'City', field_type: 'string', category: 'profile', is_system: true },
  { field_key: 'state', field_label: 'State/Province', field_type: 'string', category: 'profile', is_system: true },
  { field_key: 'country', field_label: 'Country', field_type: 'string', category: 'profile', is_system: true },
  { field_key: 'timezone', field_label: 'Timezone', field_type: 'string', category: 'profile', is_system: true },
  
  // Engagement & Behavior
  { field_key: 'engagement_score', field_label: 'Engagement Score', field_type: 'decimal', category: 'engagement', is_system: true },
  { field_key: 'total_opens', field_label: 'Total Opens', field_type: 'integer', category: 'engagement', is_system: true },
  { field_key: 'total_clicks', field_label: 'Total Clicks', field_type: 'integer', category: 'engagement', is_system: true },
  { field_key: 'total_emails_received', field_label: 'Emails Received', field_type: 'integer', category: 'engagement', is_system: true },
  { field_key: 'last_open_at', field_label: 'Last Open Date', field_type: 'datetime', category: 'engagement', is_system: true },
  { field_key: 'last_click_at', field_label: 'Last Click Date', field_type: 'datetime', category: 'engagement', is_system: true },
  { field_key: 'subscribed_at', field_label: 'Subscribed Date', field_type: 'datetime', category: 'engagement', is_system: true },
  { field_key: 'tags', field_label: 'Tags', field_type: 'tags', category: 'engagement', is_system: true },
  
  // AI/Predictions
  { field_key: 'optimal_send_hour_utc', field_label: 'Best Send Hour (UTC)', field_type: 'integer', category: 'ai', is_system: true },
  { field_key: 'churn_risk_score', field_label: 'Churn Risk Score', field_type: 'decimal', category: 'ai', is_system: true },
  { field_key: 'predicted_ltv', field_label: 'Predicted LTV', field_type: 'decimal', category: 'ai', is_system: true },
];

// Field category labels and icons
const FIELD_CATEGORIES: Record<string, { label: string; icon: string }> = {
  profile: { label: '👤 Contact Profile', icon: '👤' },
  engagement: { label: '📊 Engagement', icon: '📊' },
  ai: { label: '🤖 AI Predictions', icon: '🤖' },
  custom: { label: '📝 Custom Fields', icon: '📝' },
};

// Quick filter templates for common use cases
const QUICK_FILTERS: Array<{
  label: string;
  description: string;
  icon: string;
  condition: Partial<ConditionBuilder>;
}> = [
  {
    label: 'Gmail Users',
    description: 'Email contains gmail.com',
    icon: '📧',
    condition: { condition_type: 'profile', field: 'email', operator: 'contains', value: 'gmail.com' },
  },
  {
    label: 'Has First Name',
    description: 'First name is not empty',
    icon: '👤',
    condition: { condition_type: 'profile', field: 'first_name', operator: 'is_not_empty' },
  },
  {
    label: 'Missing First Name',
    description: 'First name is empty',
    icon: '❓',
    condition: { condition_type: 'profile', field: 'first_name', operator: 'is_empty' },
  },
  {
    label: 'Highly Engaged',
    description: 'Engagement score ≥ 70',
    icon: '🔥',
    condition: { condition_type: 'profile', field: 'engagement_score', operator: 'gte', value: '70' },
  },
  {
    label: 'At-Risk Subscribers',
    description: 'No opens in 30+ days',
    icon: '⚠️',
    condition: { condition_type: 'profile', field: 'last_open_at', operator: 'more_than_days_ago', value: '30' },
  },
  {
    label: 'New Subscribers',
    description: 'Subscribed in last 7 days',
    icon: '✨',
    condition: { condition_type: 'profile', field: 'subscribed_at', operator: 'in_last_days', value: '7' },
  },
  {
    label: 'Yahoo/Outlook Users',
    description: 'Email contains yahoo or outlook',
    icon: '📬',
    condition: { condition_type: 'profile', field: 'email', operator: 'contains', value: 'yahoo' },
  },
  {
    label: 'High Churn Risk',
    description: 'Churn risk score ≥ 0.7',
    icon: '🚨',
    condition: { condition_type: 'profile', field: 'churn_risk_score', operator: 'gte', value: '0.7' },
  },
];

export const DEFAULT_EVENTS = [
  'email_opened', 'email_clicked', 'email_delivered', 'email_sent',
  'email_bounced', 'email_unsubscribed',
  'page_view', 'form_submit', 'add_to_cart', 'purchase', 'login'
];

const TRACKING_EVENTS = new Set([
  'email_opened', 'email_clicked', 'email_delivered', 'email_sent',
  'email_bounced', 'email_unsubscribed', 'email_complained',
]);

// ==========================================
// HELPERS
// ==========================================

const generateId = () => Math.random().toString(36).substring(2, 11);

const getOperatorsForFieldType = (fieldType: FieldType): OperatorMeta[] => {
  return OPERATORS.filter(op => op.applicable_types.includes(fieldType));
};

export const createEmptyCondition = (): ConditionBuilder => ({
  id: generateId(),
  condition_type: 'profile',
  field: '',
  operator: 'equals',
  value: '',
});

export const createEmptyGroup = (): ConditionGroupBuilder => ({
  id: generateId(),
  logic_operator: 'AND',
  is_negated: false,
  conditions: [createEmptyCondition()],
  groups: [],
});

// ==========================================
// CONDITION EDITOR COMPONENT
// ==========================================

interface ConditionEditorProps {
  condition: ConditionBuilder;
  fields: ContactField[];
  events: string[];
  onChange: (condition: ConditionBuilder) => void;
  onRemove: () => void;
}

// Group fields by category for the dropdown
const groupFieldsByCategory = (fields: ContactField[]): Map<string, ContactField[]> => {
  const grouped = new Map<string, ContactField[]>();
  fields.forEach(field => {
    const category = field.category || 'other';
    if (!grouped.has(category)) {
      grouped.set(category, []);
    }
    grouped.get(category)!.push(field);
  });
  return grouped;
};

// Get placeholder text based on operator and field type
const getValuePlaceholder = (operator: Operator, fieldType: FieldType): string => {
  switch (operator) {
    case 'contains':
    case 'not_contains':
      return 'e.g., gmail.com';
    case 'starts_with':
      return 'e.g., john';
    case 'ends_with':
      return 'e.g., .com';
    case 'equals':
    case 'not_equals':
      return fieldType === 'string' ? 'exact value' : 'value';
    case 'gt':
    case 'gte':
    case 'lt':
    case 'lte':
      return 'number';
    case 'in_last_days':
    case 'in_next_days':
    case 'more_than_days_ago':
      return 'days (e.g., 30)';
    default:
      return 'value';
  }
};

const ConditionEditor: React.FC<ConditionEditorProps> = ({
  condition,
  fields,
  events,
  onChange,
  onRemove,
}) => {
  const selectedField = fields.find(f => f.field_key === condition.field);
  const fieldType = condition.condition_type === 'event' ? 'event' : (selectedField?.field_type || 'string');
  const operators = getOperatorsForFieldType(fieldType as FieldType);
  const selectedOperator = OPERATORS.find(op => op.operator === condition.operator);
  const groupedFields = groupFieldsByCategory(fields);

  const handleFieldChange = (field: string) => {
    const newField = fields.find(f => f.field_key === field);
    const newFieldType = newField?.field_type || 'string';
    const validOperators = getOperatorsForFieldType(newFieldType as FieldType);
    const newOperator = validOperators.length > 0 ? validOperators[0].operator : 'equals';
    
    onChange({
      ...condition,
      field,
      field_type: newFieldType as FieldType,
      operator: newOperator,
      condition_type: newField?.is_system ? 'profile' : 'custom_field',
    });
  };

  const handleConditionTypeChange = (type: ConditionType) => {
    onChange({
      ...condition,
      condition_type: type,
      field: type === 'event' ? '' : condition.field,
      event_name: type === 'event' ? events[0] : undefined,
    });
  };

  const selectStyle: React.CSSProperties = {
    padding: '7px 10px', fontSize: 13, borderRadius: 8,
    border: '1px solid rgba(0,200,255,0.12)', background: '#0a1020', color: '#e0e6f0',
    outline: 'none',
  };
  const inputStyle: React.CSSProperties = {
    ...selectStyle, width: 140,
  };

  return (
    <div style={{
      display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 8,
      padding: 12, borderRadius: 8,
      background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)',
    }}>
      <select
        value={condition.condition_type}
        onChange={(e) => handleConditionTypeChange(e.target.value as ConditionType)}
        style={{ ...selectStyle, fontWeight: 500 }}
      >
        <option value="profile">Contact Field</option>
        <option value="custom_field">Custom Field</option>
        <option value="event">Event</option>
        <option value="tag">Tags</option>
      </select>

      {condition.condition_type === 'event' ? (
        <select
          value={condition.event_name || ''}
          onChange={(e) => onChange({ ...condition, event_name: e.target.value })}
          style={{ ...selectStyle, minWidth: 150 }}
        >
          <option value="">Select event...</option>
          {events.map(event => (
            <option key={event} value={event}>{event.replace(/_/g, ' ')}</option>
          ))}
        </select>
      ) : condition.condition_type === 'tag' ? (
        <span style={{ padding: '7px 10px', fontSize: 13, fontWeight: 500, color: 'rgba(180,210,240,0.65)' }}>Tags</span>
      ) : (
        <select
          value={condition.field}
          onChange={(e) => handleFieldChange(e.target.value)}
          style={{ ...selectStyle, minWidth: 180 }}
        >
          <option value="">Select field...</option>
          {Array.from(groupedFields.entries()).map(([category, categoryFields]) => (
            <optgroup
              key={category}
              label={FIELD_CATEGORIES[category]?.label || category}
            >
              {categoryFields.map(field => (
                <option key={field.field_key} value={field.field_key}>
                  {field.field_label}
                </option>
              ))}
            </optgroup>
          ))}
        </select>
      )}

      <select
        value={condition.operator}
        onChange={(e) => onChange({ ...condition, operator: e.target.value as Operator })}
        style={{ ...selectStyle, minWidth: 140 }}
      >
        {operators.map(op => (
          <option key={op.operator} value={op.operator}>{op.label}</option>
        ))}
      </select>

      {selectedOperator?.requires_value && (
        <input
          type={fieldType === 'number' || fieldType === 'integer' || fieldType === 'decimal' ? 'number' : 'text'}
          value={condition.value || ''}
          onChange={(e) => onChange({ ...condition, value: e.target.value })}
          placeholder={getValuePlaceholder(condition.operator, fieldType as FieldType)}
          style={inputStyle}
        />
      )}

      {selectedOperator?.requires_secondary && (
        <>
          <span style={{ fontSize: 13, color: 'rgba(180,210,240,0.65)' }}>and</span>
          <input
            type={fieldType === 'number' || fieldType === 'integer' || fieldType === 'decimal' ? 'number' : 'text'}
            value={condition.value_secondary || ''}
            onChange={(e) => onChange({ ...condition, value_secondary: e.target.value })}
            placeholder="Value"
            style={{ ...inputStyle, width: 120 }}
          />
        </>
      )}

      {selectedOperator?.requires_array && (
        <input
          type="text"
          value={(condition.values_array || []).join(', ')}
          onChange={(e) => onChange({
            ...condition,
            values_array: e.target.value.split(',').map(v => v.trim()).filter(Boolean)
          })}
          placeholder="value1, value2, ..."
          style={{ ...inputStyle, width: 200 }}
        />
      )}

      {condition.condition_type === 'event' && (
        <div style={{ display: 'flex', alignItems: 'center', gap: 4 }}>
          <span style={{ fontSize: 13, color: 'rgba(180,210,240,0.65)' }}>in last</span>
          <input
            type="number"
            value={condition.event_time_window_days || ''}
            onChange={(e) => onChange({ ...condition, event_time_window_days: parseInt(e.target.value) || undefined })}
            placeholder="days"
            style={{ ...inputStyle, width: 60 }}
          />
          <span style={{ fontSize: 13, color: 'rgba(180,210,240,0.65)' }}>days</span>
        </div>
      )}

      {condition.condition_type === 'event' && TRACKING_EVENTS.has(condition.event_name || '') && (
        <div style={{ display: 'flex', alignItems: 'center', gap: 4 }}>
          <span style={{ fontSize: 13, color: 'rgba(180,210,240,0.65)' }}>from domain</span>
          <input
            type="text"
            value={condition.event_sending_domain || ''}
            onChange={(e) => onChange({ ...condition, event_sending_domain: e.target.value })}
            placeholder="e.g. discountblog.com"
            style={{ ...inputStyle, width: 180 }}
          />
        </div>
      )}

      <button
        type="button"
        onClick={onRemove}
        style={{
          padding: 6, background: 'rgba(233,69,96,0.1)', border: 'none',
          borderRadius: 6, cursor: 'pointer', color: '#e94560',
          display: 'flex', alignItems: 'center', justifyContent: 'center',
        }}
        title="Remove condition"
      >
        <svg width={14} height={14} fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M6 18L18 6M6 6l12 12" />
        </svg>
      </button>
    </div>
  );
};

// ==========================================
// CONDITION GROUP COMPONENT
// ==========================================

interface ConditionGroupEditorProps {
  group: ConditionGroupBuilder;
  fields: ContactField[];
  events: string[];
  depth: number;
  onChange: (group: ConditionGroupBuilder) => void;
  onRemove?: () => void;
}

const groupBorders = [
  'rgba(0,200,255,0.12)',
  'rgba(0,140,255,0.18)',
  'rgba(0,184,148,0.18)',
  'rgba(140,100,255,0.18)',
];
const groupBgs = [
  'rgba(10,16,32,0.6)',
  'rgba(0,80,180,0.06)',
  'rgba(0,140,100,0.06)',
  'rgba(100,60,200,0.06)',
];

export const ConditionGroupEditor: React.FC<ConditionGroupEditorProps> = ({
  group,
  fields,
  events,
  depth,
  onChange,
  onRemove,
}) => {
  const borderClr = groupBorders[depth % groupBorders.length];
  const bgClr = groupBgs[depth % groupBgs.length];

  const handleConditionChange = (index: number, condition: ConditionBuilder) => {
    const newConditions = [...group.conditions];
    newConditions[index] = condition;
    onChange({ ...group, conditions: newConditions });
  };

  const handleConditionRemove = (index: number) => {
    const newConditions = group.conditions.filter((_, i) => i !== index);
    onChange({ ...group, conditions: newConditions });
  };

  const handleAddCondition = () => {
    onChange({
      ...group,
      conditions: [...group.conditions, createEmptyCondition()],
    });
  };

  const handleChildGroupChange = (index: number, childGroup: ConditionGroupBuilder) => {
    const newGroups = [...group.groups];
    newGroups[index] = childGroup;
    onChange({ ...group, groups: newGroups });
  };

  const handleChildGroupRemove = (index: number) => {
    const newGroups = group.groups.filter((_, i) => i !== index);
    onChange({ ...group, groups: newGroups });
  };

  const handleAddGroup = () => {
    onChange({
      ...group,
      groups: [...group.groups, createEmptyGroup()],
    });
  };

  const toggleLogicOperator = () => {
    onChange({
      ...group,
      logic_operator: group.logic_operator === 'AND' ? 'OR' : 'AND',
    });
  };

  const toggleNegation = () => {
    onChange({
      ...group,
      is_negated: !group.is_negated,
    });
  };

  return (
    <div
      style={{
        borderRadius: 10,
        border: `2px solid ${borderClr}`,
        background: bgClr,
        padding: 16,
      }}
    >
      {/* Group Header */}
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 12 }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          {group.is_negated && (
            <span style={{
              padding: '2px 8px', fontSize: 11, fontWeight: 600, borderRadius: 4,
              background: 'rgba(233,69,96,0.15)', color: '#e94560',
            }}>NOT</span>
          )}
          <button
            type="button"
            onClick={toggleLogicOperator}
            style={{
              padding: '6px 14px', fontSize: 13, fontWeight: 700, borderRadius: 6, border: 'none',
              cursor: 'pointer', transition: 'all 0.2s', letterSpacing: 1,
              background: group.logic_operator === 'AND' ? 'rgba(0,229,255,0.15)' : 'rgba(255,160,50,0.18)',
              color: group.logic_operator === 'AND' ? '#00e5ff' : '#ffb347',
              boxShadow: group.logic_operator === 'AND'
                ? '0 0 8px rgba(0,229,255,0.2)' : '0 0 8px rgba(255,160,50,0.2)',
            }}
          >
            {group.logic_operator}
          </button>
          <span style={{ fontSize: 13, color: 'rgba(180,210,240,0.65)' }}>
            Match {group.logic_operator === 'AND' ? 'all' : 'any'} of the following
          </span>
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: 4 }}>
          <button
            type="button"
            onClick={toggleNegation}
            style={{
              padding: '4px 10px', fontSize: 11, borderRadius: 4, border: 'none', cursor: 'pointer',
              background: group.is_negated ? 'rgba(233,69,96,0.15)' : 'rgba(0,200,255,0.08)',
              color: group.is_negated ? '#e94560' : 'rgba(180,210,240,0.65)',
            }}
          >
            {group.is_negated ? 'Negated' : 'Negate'}
          </button>
          {onRemove && depth > 0 && (
            <button
              type="button"
              onClick={onRemove}
              style={{
                padding: 4, background: 'none', border: 'none', cursor: 'pointer',
                color: 'rgba(180,210,240,0.5)', borderRadius: 4,
              }}
              title="Remove group"
            >
              <svg width={16} height={16} fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M6 18L18 6M6 6l12 12" />
              </svg>
            </button>
          )}
        </div>
      </div>

      {/* Conditions */}
      <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
        {group.conditions.map((condition, index) => (
          <div key={condition.id} style={{ display: 'flex', alignItems: 'flex-start' }}>
            {index > 0 && (
              <span style={{
                marginRight: 8, marginTop: 12, fontSize: 11, fontWeight: 600,
                color: group.logic_operator === 'AND' ? '#00e5ff' : '#ffb347',
              }}>
                {group.logic_operator}
              </span>
            )}
            <div style={{ flex: 1 }}>
              <ConditionEditor
                condition={condition}
                fields={fields}
                events={events}
                onChange={(c) => handleConditionChange(index, c)}
                onRemove={() => handleConditionRemove(index)}
              />
            </div>
          </div>
        ))}

        {/* Nested Groups */}
        {group.groups.map((childGroup, index) => (
          <div key={childGroup.id} style={{ marginTop: 8 }}>
            {(group.conditions.length > 0 || index > 0) && (
              <span style={{
                fontSize: 11, fontWeight: 600,
                color: group.logic_operator === 'AND' ? '#00e5ff' : '#ffb347',
              }}>
                {group.logic_operator}
              </span>
            )}
            <ConditionGroupEditor
              group={childGroup}
              fields={fields}
              events={events}
              depth={depth + 1}
              onChange={(g) => handleChildGroupChange(index, g)}
              onRemove={() => handleChildGroupRemove(index)}
            />
          </div>
        ))}
      </div>

      {/* Add Buttons */}
      <div style={{ display: 'flex', gap: 8, marginTop: 12 }}>
        <button
          type="button"
          onClick={handleAddCondition}
          style={{
            display: 'flex', alignItems: 'center', gap: 6,
            padding: '7px 14px', fontSize: 13, fontWeight: 500, borderRadius: 8,
            border: '1px solid rgba(0,229,255,0.15)', cursor: 'pointer',
            background: 'rgba(0,229,255,0.08)', color: '#00e5ff',
            transition: 'all 0.2s',
          }}
        >
          <svg width={14} height={14} fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M12 4v16m8-8H4" />
          </svg>
          Add Condition
        </button>
        <button
          type="button"
          onClick={handleAddGroup}
          style={{
            display: 'flex', alignItems: 'center', gap: 6,
            padding: '7px 14px', fontSize: 13, fontWeight: 500, borderRadius: 8,
            border: '1px solid rgba(140,100,255,0.2)', cursor: 'pointer',
            background: 'rgba(140,100,255,0.08)', color: '#b18cff',
            transition: 'all 0.2s',
          }}
        >
          <svg width={14} height={14} fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M19 11H5m14 0a2 2 0 012 2v6a2 2 0 01-2 2H5a2 2 0 01-2-2v-6a2 2 0 012-2m14 0V9a2 2 0 00-2-2M5 11V9a2 2 0 012-2m0 0V5a2 2 0 012-2h6a2 2 0 012 2v2M7 7h10" />
          </svg>
          Add Group
        </button>
      </div>
    </div>
  );
};

// ==========================================
// SEGMENT PREVIEW COMPONENT
// ==========================================

interface SegmentPreviewProps {
  preview: SegmentPreview | null;
  loading: boolean;
}

const SegmentPreviewPanel: React.FC<SegmentPreviewProps> = ({ preview, loading }) => {
  if (loading) {
    return (
      <div className="p-4 bg-gray-50 rounded-lg border border-gray-200">
        <div className="flex items-center gap-2">
          <div className="animate-spin w-4 h-4 border-2 border-blue-500 border-t-transparent rounded-full"></div>
          <span className="text-sm text-gray-500">Calculating segment...</span>
        </div>
      </div>
    );
  }

  if (!preview) {
    return (
      <div className="p-4 bg-gray-50 rounded-lg border border-gray-200">
        <p className="text-sm text-gray-500">Add conditions to see a preview</p>
      </div>
    );
  }

  return (
    <div className="bg-white rounded-lg border border-gray-200 overflow-hidden">
      {/* Count Header */}
      <div className="px-4 py-3 bg-gradient-to-r from-blue-500 to-purple-500">
        <div className="text-3xl font-bold text-white">
          {preview.estimated_count.toLocaleString()}
        </div>
        <div className="text-sm text-blue-100">matching contacts</div>
      </div>

      {/* Sample Subscribers */}
      {preview.sample_subscribers.length > 0 && (
        <div className="p-4">
          <h4 className="text-sm font-medium text-gray-700 mb-2">Sample contacts</h4>
          <div className="space-y-2">
            {preview.sample_subscribers.map((sub) => (
              <div key={sub.id} className="flex items-center justify-between p-2 bg-gray-50 rounded">
                <div>
                  <div className="text-sm font-medium text-gray-900">
                    {sub.first_name || sub.last_name 
                      ? `${sub.first_name || ''} ${sub.last_name || ''}`.trim()
                      : sub.email}
                  </div>
                  <div className="text-xs text-gray-500">{sub.email}</div>
                </div>
                <div className="text-right">
                  <div className="text-sm font-medium text-gray-700">
                    {sub.engagement_score.toFixed(0)}
                  </div>
                  <div className="text-xs text-gray-500">score</div>
                </div>
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  );
};

// ==========================================
// MAIN SEGMENT BUILDER COMPONENT
// ==========================================

interface SegmentBuilderProps {
  initialConditions?: ConditionGroupBuilder;
  listId?: string;
  onSave?: (name: string, conditions: ConditionGroupBuilder) => void;
  onChange?: (conditions: ConditionGroupBuilder) => void;
}

export const SegmentBuilder: React.FC<SegmentBuilderProps> = ({
  initialConditions,
  listId,
  onSave,
  onChange,
}) => {
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const [rootGroup, setRootGroup] = useState<ConditionGroupBuilder>(
    initialConditions || createEmptyGroup()
  );
  const [fields, setFields] = useState<ContactField[]>(DEFAULT_FIELDS);
  const [events] = useState<string[]>(DEFAULT_EVENTS);
  const [preview, setPreview] = useState<SegmentPreview | null>(null);
  const [previewLoading, setPreviewLoading] = useState(false);

  // Load custom fields from API
  useEffect(() => {
    const loadFields = async () => {
      try {
        const response = await fetch('/api/mailing/v2/contact-fields');
        if (response.ok) {
          const customFields = await response.json();
          if (Array.isArray(customFields)) {
            setFields([...DEFAULT_FIELDS, ...customFields]);
          }
        }
      } catch (error) {
        console.error('Failed to load contact fields:', error);
      }
    };
    loadFields();
  }, []);

  // Debounced preview calculation
  const calculatePreview = useCallback(async () => {
    setPreviewLoading(true);
    try {
      const response = await fetch('/api/mailing/v2/segments/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          list_id: listId,
          root_group: rootGroup,
          limit: 5,
        }),
      });
      if (response.ok) {
        const data = await response.json();
        setPreview(data);
      }
    } catch (error) {
      console.error('Failed to calculate preview:', error);
    }
    setPreviewLoading(false);
  }, [rootGroup, listId]);

  // Calculate preview on conditions change (debounced)
  useEffect(() => {
    const timer = setTimeout(() => {
      if (rootGroup.conditions.length > 0 && rootGroup.conditions[0].field) {
        calculatePreview();
      }
    }, 500);
    return () => clearTimeout(timer);
  }, [rootGroup, calculatePreview]);

  // Notify parent of changes
  useEffect(() => {
    onChange?.(rootGroup);
  }, [rootGroup, onChange]);

  const handleSave = () => {
    if (!name.trim()) {
      alert('Please enter a segment name');
      return;
    }
    onSave?.(name, rootGroup);
  };

  return (
    <div className="flex gap-6">
      {/* Main Editor */}
      <div className="flex-1">
        {/* Name & Description */}
        <div className="mb-6 space-y-4">
          <div>
            <label className="block text-sm font-medium text-gray-700 mb-1">
              Segment Name
            </label>
            <input
              type="text"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="e.g., High-Value Engaged Customers"
              className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
            />
          </div>
          <div>
            <label className="block text-sm font-medium text-gray-700 mb-1">
              Description (optional)
            </label>
            <textarea
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="Describe who this segment targets..."
              rows={2}
              className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
            />
          </div>
        </div>

        {/* Quick Filters - Common Segment Templates */}
        <div className="mb-6">
          <h3 className="text-lg font-medium text-gray-900 mb-2">Quick Filters</h3>
          <p className="text-sm text-gray-500 mb-3">Click to add a pre-built condition</p>
          <div className="flex flex-wrap gap-2">
            {QUICK_FILTERS.map((filter, index) => (
              <button
                type="button"
                key={index}
                onClick={() => {
                  const newCondition: ConditionBuilder = {
                    id: generateId(),
                    condition_type: filter.condition.condition_type || 'profile',
                    field: filter.condition.field || '',
                    operator: filter.condition.operator || 'equals',
                    value: filter.condition.value,
                  };
                  setRootGroup({
                    ...rootGroup,
                    conditions: [...rootGroup.conditions, newCondition],
                  });
                }}
                className="flex items-center gap-1.5 px-3 py-1.5 text-sm bg-white border border-gray-200 rounded-full hover:bg-gray-50 hover:border-blue-300 transition-colors"
                title={filter.description}
              >
                <span>{filter.icon}</span>
                <span>{filter.label}</span>
              </button>
            ))}
          </div>
        </div>

        {/* Conditions Editor */}
        <div className="mb-6">
          <h3 className="text-lg font-medium text-gray-900 mb-3">Conditions</h3>
          <ConditionGroupEditor
            group={rootGroup}
            fields={fields}
            events={events}
            depth={0}
            onChange={setRootGroup}
          />
        </div>

        {/* Save Button */}
        {onSave && (
          <button
            type="button"
            onClick={handleSave}
            className="px-6 py-2 bg-blue-600 text-white font-medium rounded-lg hover:bg-blue-700 transition-colors"
          >
            Save Segment
          </button>
        )}
      </div>

      {/* Preview Sidebar */}
      <div className="w-80 shrink-0">
        <h3 className="text-lg font-medium text-gray-900 mb-3">Preview</h3>
        <SegmentPreviewPanel preview={preview} loading={previewLoading} />
      </div>
    </div>
  );
};

export default SegmentBuilder;
