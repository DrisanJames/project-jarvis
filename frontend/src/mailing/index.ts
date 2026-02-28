// Main portal
export { MailingPortal } from './pages/MailingPortal';

// Components
export { MailingDashboard } from './components/MailingDashboard';
export { ListsManager } from './components/ListsManager';
export { CampaignsManager } from './components/CampaignsManager';
export { SendingPlans } from './components/SendingPlans';

// Hooks
export {
  useDashboard,
  useLists,
  useSubscribers,
  useCampaigns,
  useCampaign,
  useDeliveryServers,
  useSendingPlans,
} from './hooks/useMailingApi';

// Types
export * from './types';
