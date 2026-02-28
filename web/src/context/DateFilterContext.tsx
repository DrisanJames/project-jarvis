import React, { createContext, useContext, useState, useMemo, useCallback } from 'react';

export type DateRangeType = 'today' | 'last24h' | 'last7' | 'last14' | 'mtd' | 'last30' | 'last60' | 'last90' | 'lastMonth' | 'ytd' | 'custom';

export interface DateRange {
  type: DateRangeType;
  startDate: string; // ISO format: YYYY-MM-DD
  endDate: string;   // ISO format: YYYY-MM-DD
  label: string;
}

interface DateFilterContextType {
  dateRange: DateRange;
  setDateRangeType: (type: DateRangeType) => void;
  setCustomRange: (startDate: string, endDate: string) => void;
}

const DateFilterContext = createContext<DateFilterContextType | undefined>(undefined);

// Helper to format date as YYYY-MM-DD
const formatDate = (date: Date): string => {
  return date.toISOString().split('T')[0];
};

// Calculate date range based on type
const calculateDateRange = (type: DateRangeType, customStart?: string, customEnd?: string): DateRange => {
  const now = new Date();

  switch (type) {
    case 'today': {
      return { type: 'today', startDate: formatDate(now), endDate: formatDate(now), label: 'Today' };
    }
    case 'last24h': {
      const d = new Date(now);
      d.setDate(d.getDate() - 1);
      return { type: 'last24h', startDate: formatDate(d), endDate: formatDate(now), label: 'Last 24 Hours' };
    }
    case 'last7': {
      const d = new Date(now);
      d.setDate(d.getDate() - 7);
      return { type: 'last7', startDate: formatDate(d), endDate: formatDate(now), label: 'Last 7 Days' };
    }
    case 'last14': {
      const d = new Date(now);
      d.setDate(d.getDate() - 14);
      return { type: 'last14', startDate: formatDate(d), endDate: formatDate(now), label: 'Last 14 Days' };
    }
    case 'mtd': {
      const startOfMonth = new Date(now.getFullYear(), now.getMonth(), 1);
      return { type: 'mtd', startDate: formatDate(startOfMonth), endDate: formatDate(now), label: 'Month to Date' };
    }
    case 'last30': {
      const d = new Date(now);
      d.setDate(d.getDate() - 30);
      return { type: 'last30', startDate: formatDate(d), endDate: formatDate(now), label: 'Last 30 Days' };
    }
    case 'last60': {
      const d = new Date(now);
      d.setDate(d.getDate() - 60);
      return { type: 'last60', startDate: formatDate(d), endDate: formatDate(now), label: 'Last 60 Days' };
    }
    case 'last90': {
      const d = new Date(now);
      d.setDate(d.getDate() - 90);
      return { type: 'last90', startDate: formatDate(d), endDate: formatDate(now), label: 'Last 90 Days' };
    }
    case 'lastMonth': {
      const lastMonthEnd = new Date(now.getFullYear(), now.getMonth(), 0);
      const lastMonthStart = new Date(lastMonthEnd.getFullYear(), lastMonthEnd.getMonth(), 1);
      return { type: 'lastMonth', startDate: formatDate(lastMonthStart), endDate: formatDate(lastMonthEnd), label: 'Last Month' };
    }
    case 'ytd': {
      const startOfYear = new Date(now.getFullYear(), 0, 1);
      return { type: 'ytd', startDate: formatDate(startOfYear), endDate: formatDate(now), label: 'Year to Date' };
    }
    case 'custom': {
      return {
        type: 'custom',
        startDate: customStart || formatDate(now),
        endDate: customEnd || formatDate(now),
        label: 'Custom Range',
      };
    }
  }
};

export const DateFilterProvider: React.FC<{ children: React.ReactNode }> = ({ children }) => {
  const [dateRangeType, setDateRangeTypeState] = useState<DateRangeType>('last30');
  const [customStart, setCustomStart] = useState<string>('');
  const [customEnd, setCustomEnd] = useState<string>('');

  const dateRange = useMemo(
    () => calculateDateRange(dateRangeType, customStart, customEnd),
    [dateRangeType, customStart, customEnd],
  );

  const setDateRangeType = useCallback((type: DateRangeType) => {
    setDateRangeTypeState(type);
  }, []);

  const setCustomRange = useCallback((start: string, end: string) => {
    setCustomStart(start);
    setCustomEnd(end);
    setDateRangeTypeState('custom');
  }, []);

  const value = useMemo(() => ({
    dateRange,
    setDateRangeType,
    setCustomRange,
  }), [dateRange, setDateRangeType, setCustomRange]);

  return (
    <DateFilterContext.Provider value={value}>
      {children}
    </DateFilterContext.Provider>
  );
};

export const useDateFilter = (): DateFilterContextType => {
  const context = useContext(DateFilterContext);
  if (!context) {
    throw new Error('useDateFilter must be used within a DateFilterProvider');
  }
  return context;
};

// Helper hook to build API URL with date range params
export const useApiUrlWithDateRange = (baseUrl: string): string => {
  const { dateRange } = useDateFilter();
  const separator = baseUrl.includes('?') ? '&' : '?';
  return `${baseUrl}${separator}start_date=${dateRange.startDate}&end_date=${dateRange.endDate}&range_type=${dateRange.type}`;
};
