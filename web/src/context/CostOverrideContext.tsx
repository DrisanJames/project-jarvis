import React, { createContext, useContext, useState, useCallback, useEffect, ReactNode } from 'react';

// Types
export interface CostOverride {
  category: string;
  name: string;
  new_cost: number;
  original_cost: number;
}

export interface CostChangeLog {
  id: string;
  timestamp: string;
  name: string;
  category: string;
  old_value: number;
  new_value: number;
  saved: boolean;
}

interface CostOverrideContextType {
  // State
  costOverrides: CostOverride[];
  hasUnsavedChanges: boolean;
  isSaving: boolean;
  saveError: string | null;
  changeLog: CostChangeLog[];
  
  // Actions
  setOverride: (override: CostOverride) => void;
  resetOverride: (name: string, category: string) => void;
  resetAllOverrides: () => void;
  saveOverrides: () => Promise<boolean>;
  loadPersistedOverrides: () => Promise<void>;
  
  // Helpers
  getOverride: (name: string, category: string) => CostOverride | undefined;
  getCostWithOverride: (name: string, category: string, originalCost: number) => number;
  getTotalCostChange: () => number;
}

const CostOverrideContext = createContext<CostOverrideContextType | undefined>(undefined);

interface CostOverrideProviderProps {
  children: ReactNode;
}

export const CostOverrideProvider: React.FC<CostOverrideProviderProps> = ({ children }) => {
  const [costOverrides, setCostOverrides] = useState<CostOverride[]>([]);
  const [hasUnsavedChanges, setHasUnsavedChanges] = useState(false);
  const [isSaving, setIsSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [changeLog, setChangeLog] = useState<CostChangeLog[]>([]);
  const [isLoaded, setIsLoaded] = useState(false);

  // Load persisted overrides on mount
  const loadPersistedOverrides = useCallback(async () => {
    try {
      const response = await fetch('/api/financial/config/costs', {
        credentials: 'include',
      });
      if (response.ok) {
        const result = await response.json();
        if (result.configs) {
          const loadedOverrides: CostOverride[] = [];
          Object.values(result.configs).forEach((items: unknown) => {
            (items as Array<{ 
              is_overridden: boolean; 
              name: string; 
              type: string; 
              monthly_cost: number; 
              original_cost: number 
            }>)
              .filter((item) => item.is_overridden)
              .forEach((item) => {
                loadedOverrides.push({
                  name: item.name,
                  category: item.type,
                  new_cost: item.monthly_cost,
                  original_cost: item.original_cost,
                });
              });
          });
          if (loadedOverrides.length > 0) {
            setCostOverrides(loadedOverrides);
          }
        }
      }
    } catch (err) {
      console.error('Failed to load persisted cost configs:', err);
    } finally {
      setIsLoaded(true);
    }
  }, []);

  // Load on mount
  useEffect(() => {
    if (!isLoaded) {
      loadPersistedOverrides();
    }
  }, [isLoaded, loadPersistedOverrides]);

  // Set a single override
  const setOverride = useCallback((override: CostOverride) => {
    setCostOverrides(prev => {
      const existing = prev.findIndex(o => o.name === override.name && o.category === override.category);
      
      // Log the change
      const logEntry: CostChangeLog = {
        id: `${Date.now()}-${override.name}`,
        timestamp: new Date().toISOString(),
        name: override.name,
        category: override.category,
        old_value: existing >= 0 ? prev[existing].new_cost : override.original_cost,
        new_value: override.new_cost,
        saved: false,
      };
      setChangeLog(logs => [logEntry, ...logs].slice(0, 100)); // Keep last 100 entries
      
      if (existing >= 0) {
        const updated = [...prev];
        updated[existing] = override;
        return updated;
      }
      return [...prev, override];
    });
    setHasUnsavedChanges(true);
    setSaveError(null);
  }, []);

  // Reset a single override
  const resetOverride = useCallback((name: string, category: string) => {
    setCostOverrides(prev => {
      const override = prev.find(o => o.name === name && o.category === category);
      if (override) {
        // Log the reset
        const logEntry: CostChangeLog = {
          id: `${Date.now()}-reset-${name}`,
          timestamp: new Date().toISOString(),
          name,
          category,
          old_value: override.new_cost,
          new_value: override.original_cost,
          saved: false,
        };
        setChangeLog(logs => [logEntry, ...logs].slice(0, 100));
      }
      return prev.filter(o => !(o.name === name && o.category === category));
    });
    setHasUnsavedChanges(true);
  }, []);

  // Reset all overrides
  const resetAllOverrides = useCallback(() => {
    if (costOverrides.length > 0) {
      const logEntry: CostChangeLog = {
        id: `${Date.now()}-reset-all`,
        timestamp: new Date().toISOString(),
        name: 'All Costs',
        category: 'all',
        old_value: 0,
        new_value: 0,
        saved: false,
      };
      setChangeLog(logs => [logEntry, ...logs].slice(0, 100));
    }
    setCostOverrides([]);
    setHasUnsavedChanges(false);
    setSaveError(null);
  }, [costOverrides.length]);

  // Save overrides to backend
  const saveOverrides = useCallback(async (): Promise<boolean> => {
    setIsSaving(true);
    setSaveError(null);

    try {
      // First, get the current cost breakdown to build the full config
      const breakdownResponse = await fetch('/api/financial/dashboard', {
        credentials: 'include',
      });
      
      if (!breakdownResponse.ok) {
        throw new Error('Failed to fetch current cost data');
      }
      
      const breakdownData = await breakdownResponse.json();
      const costBreakdown = breakdownData.cost_breakdown;
      
      if (!costBreakdown) {
        throw new Error('No cost breakdown data available');
      }

      // Build the cost items with overrides applied
      const buildCostItems = (items: Array<{ name: string; category: string; monthly_cost: number }>, type: string) => {
        return items.map(item => {
          const override = costOverrides.find(o => o.name === item.name && o.category === type);
          return {
            name: item.name,
            category: item.category,
            type: type,
            monthly_cost: override?.new_cost ?? item.monthly_cost,
            original_cost: override?.original_cost ?? item.monthly_cost,
            is_overridden: !!override && override.new_cost !== (override.original_cost ?? item.monthly_cost),
          };
        });
      };

      const configData: Record<string, unknown[]> = {
        vendor: buildCostItems(costBreakdown.vendor_costs || [], 'vendor'),
        esp: buildCostItems(costBreakdown.esp_costs || [], 'esp'),
        payroll: buildCostItems(costBreakdown.payroll_costs || [], 'payroll'),
        revenue_share: buildCostItems(costBreakdown.revenue_share_costs || [], 'revenue_share'),
      };

      const response = await fetch('/api/financial/config/costs', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'include',
        body: JSON.stringify(configData),
      });

      if (response.ok) {
        // Mark all change log entries as saved
        setChangeLog(logs => logs.map(log => ({ ...log, saved: true })));
        setHasUnsavedChanges(false);
        return true;
      } else {
        const err = await response.json();
        setSaveError(err.error || 'Failed to save configurations');
        return false;
      }
    } catch (err) {
      const errorMessage = err instanceof Error ? err.message : 'Failed to save configurations';
      setSaveError(errorMessage);
      console.error('Failed to save cost configs:', err);
      return false;
    } finally {
      setIsSaving(false);
    }
  }, [costOverrides]);

  // Get a specific override
  const getOverride = useCallback((name: string, category: string): CostOverride | undefined => {
    return costOverrides.find(o => o.name === name && o.category === category);
  }, [costOverrides]);

  // Get cost with override applied
  const getCostWithOverride = useCallback((name: string, category: string, originalCost: number): number => {
    const override = costOverrides.find(o => o.name === name && o.category === category);
    return override?.new_cost ?? originalCost;
  }, [costOverrides]);

  // Calculate total cost change
  const getTotalCostChange = useCallback((): number => {
    return costOverrides.reduce((total, override) => {
      return total + (override.new_cost - override.original_cost);
    }, 0);
  }, [costOverrides]);

  const value: CostOverrideContextType = {
    costOverrides,
    hasUnsavedChanges,
    isSaving,
    saveError,
    changeLog,
    setOverride,
    resetOverride,
    resetAllOverrides,
    saveOverrides,
    loadPersistedOverrides,
    getOverride,
    getCostWithOverride,
    getTotalCostChange,
  };

  return (
    <CostOverrideContext.Provider value={value}>
      {children}
    </CostOverrideContext.Provider>
  );
};

// Custom hook to use the context
export const useCostOverrides = (): CostOverrideContextType => {
  const context = useContext(CostOverrideContext);
  if (context === undefined) {
    throw new Error('useCostOverrides must be used within a CostOverrideProvider');
  }
  return context;
};

export default CostOverrideContext;
