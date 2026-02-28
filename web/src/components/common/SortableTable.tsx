import React, { useState, useMemo, useCallback } from 'react';

export type SortDirection = 'asc' | 'desc' | null;

export interface SortableColumn<T> {
  key: string;
  header: string;
  render?: (item: T) => React.ReactNode;
  align?: 'left' | 'center' | 'right';
  width?: string;
  sortable?: boolean;
  sortKey?: string; // Key to use for sorting if different from display key
  sortFn?: (a: T, b: T) => number; // Custom sort function
}

interface SortableTableProps<T> {
  columns: SortableColumn<T>[];
  data: T[];
  keyExtractor: (item: T, index: number) => string;
  onRowClick?: (item: T) => void;
  emptyMessage?: string;
  defaultSortKey?: string;
  defaultSortDirection?: SortDirection;
  stickyHeader?: boolean;
  maxHeight?: string;
}

export function SortableTable<T>({
  columns,
  data,
  keyExtractor,
  onRowClick,
  emptyMessage = 'No data available',
  defaultSortKey,
  defaultSortDirection = 'desc',
  stickyHeader = false,
  maxHeight,
}: SortableTableProps<T>) {
  const [sortKey, setSortKey] = useState<string | null>(defaultSortKey || null);
  const [sortDirection, setSortDirection] = useState<SortDirection>(
    defaultSortKey ? defaultSortDirection : null
  );

  const getValue = useCallback((item: T, key: string): unknown => {
    const keys = key.split('.');
    let value: unknown = item;
    for (const k of keys) {
      if (value && typeof value === 'object' && k in value) {
        value = (value as Record<string, unknown>)[k];
      } else {
        return undefined;
      }
    }
    return value;
  }, []);

  const handleSort = useCallback((column: SortableColumn<T>) => {
    if (column.sortable === false) return;
    
    const key = column.sortKey || column.key;
    
    if (sortKey === key) {
      // Toggle direction or clear sort
      if (sortDirection === 'asc') {
        setSortDirection('desc');
      } else if (sortDirection === 'desc') {
        setSortDirection(null);
        setSortKey(null);
      } else {
        setSortDirection('asc');
      }
    } else {
      setSortKey(key);
      setSortDirection('desc');
    }
  }, [sortKey, sortDirection]);

  const sortedData = useMemo(() => {
    if (!sortKey || !sortDirection) return data;

    const column = columns.find(c => (c.sortKey || c.key) === sortKey);
    
    return [...data].sort((a, b) => {
      // Use custom sort function if provided
      if (column?.sortFn) {
        const result = column.sortFn(a, b);
        return sortDirection === 'asc' ? result : -result;
      }

      const aVal = getValue(a, sortKey);
      const bVal = getValue(b, sortKey);

      // Handle null/undefined
      if (aVal == null && bVal == null) return 0;
      if (aVal == null) return 1;
      if (bVal == null) return -1;

      // Compare values
      let result = 0;
      if (typeof aVal === 'number' && typeof bVal === 'number') {
        result = aVal - bVal;
      } else if (typeof aVal === 'string' && typeof bVal === 'string') {
        result = aVal.localeCompare(bVal);
      } else {
        result = String(aVal).localeCompare(String(bVal));
      }

      return sortDirection === 'asc' ? result : -result;
    });
  }, [data, sortKey, sortDirection, columns, getValue]);

  const getSortIndicator = (column: SortableColumn<T>) => {
    if (column.sortable === false) return null;
    const key = column.sortKey || column.key;
    if (sortKey !== key) return ' ↕';
    return sortDirection === 'asc' ? ' ↑' : ' ↓';
  };

  if (data.length === 0) {
    return (
      <div className="table-container">
        <div className="empty-state">{emptyMessage}</div>
      </div>
    );
  }

  return (
    <div 
      className="table-container" 
      style={{ 
        overflowX: 'auto',
        maxHeight: maxHeight,
        overflowY: maxHeight ? 'auto' : undefined,
      }}
    >
      <table className="table sortable-table">
        <thead>
          <tr>
            {columns.map((column) => (
              <th
                key={column.key}
                onClick={() => handleSort(column)}
                style={{
                  textAlign: column.align || 'left',
                  width: column.width,
                  cursor: column.sortable !== false ? 'pointer' : 'default',
                  userSelect: 'none',
                  position: stickyHeader ? 'sticky' : undefined,
                  top: stickyHeader ? 0 : undefined,
                  backgroundColor: stickyHeader ? 'var(--card-bg, #fff)' : undefined,
                  zIndex: stickyHeader ? 1 : undefined,
                }}
                className={column.sortable !== false ? 'sortable-header' : ''}
              >
                {column.header}
                {getSortIndicator(column)}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {sortedData.map((item, index) => (
            <tr
              key={keyExtractor(item, index)}
              onClick={() => onRowClick?.(item)}
              style={{ cursor: onRowClick ? 'pointer' : 'default' }}
            >
              {columns.map((column) => (
                <td
                  key={column.key}
                  style={{ textAlign: column.align || 'left' }}
                >
                  {column.render
                    ? column.render(item)
                    : String(getValue(item, column.key) ?? '-')}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
      <style>{`
        .sortable-table .sortable-header:hover {
          background-color: var(--hover-bg, #f5f5f5);
        }
        .empty-state {
          padding: 2rem;
          text-align: center;
          color: var(--text-muted, #666);
        }
      `}</style>
    </div>
  );
}

export default SortableTable;
