import React, { Suspense, lazy } from 'react';
import { GoogleLogin } from './components/auth';

// Lazy so signed-out marketing visitors never download the portal bundle.
const MailingPortal = lazy(() =>
  import('./components/mailing/pages/MailingPortal').then((m) => ({ default: m.MailingPortal })),
);
import { AuthProvider } from './contexts/AuthContext';
import { DateFilterProvider } from './context/DateFilterContext';
import { CostOverrideProvider } from './context/CostOverrideContext';
import { ThemeProvider } from './context/ThemeContext';

// PMTA-only mode: App renders the Mailing Portal directly.
// To re-enable the full analytics platform, restore the commented-out
// imports above and the analytics layout below.

const App: React.FC = () => {
  return (
    <ThemeProvider>
      <AuthProvider>
        <GoogleLogin>
          <DateFilterProvider>
            <CostOverrideProvider>
              <Suspense fallback={null}>
                <MailingPortal />
              </Suspense>
            </CostOverrideProvider>
          </DateFilterProvider>
        </GoogleLogin>
      </AuthProvider>
    </ThemeProvider>
  );
};

export default App;
