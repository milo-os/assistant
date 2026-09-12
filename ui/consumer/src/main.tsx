import ChatDock from './widgets/chat-dock';
import type { PortalPluginHostBindings } from '@datum-cloud/portal-plugin-sdk/host';
import { PortalPluginHostProvider } from '@datum-cloud/portal-plugin-sdk/host';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { StrictMode, useCallback } from 'react';
import { createRoot } from 'react-dom/client';
import { MemoryRouter } from 'react-router';

// Standalone preview only. Wraps ChatDock in a MemoryRouter (matching
// compute's ui/consumer/src/main.tsx pattern, in case the widget ever needs
// routing context) and a QueryClientProvider, then provides a local
// `PortalPluginHostProvider` — the same wiring the real portal host supplies
// (see cloud-portal's `plugin-sdk-bindings.tsx`), but backed by a plain
// `fetch` against a directly reachable apiserver instead of the portal's
// Milo proxy.
//
// `apiBase` points at a locally port-forwarded/dev `cmd/assistant-apiserver`.
// The assistant repo's dev loop doesn't yet document a conventional apiserver
// port (only the standalone A2A server's `task dev:forward` → localhost:1986
// is documented in the README as of this writing) — override with
// `VITE_ASSISTANT_APISERVER_URL` once one is settled, or run
// `kubectl port-forward svc/assistant-apiserver <port>:443` yourself.
const apiBase =
  (import.meta.env as unknown as Record<string, string | undefined>)
    .VITE_ASSISTANT_APISERVER_URL ?? 'http://localhost:8443';

const DEMO_PROJECT = { name: 'demo-project', displayName: 'Demo Project' };

function useStandalonePluginFetch() {
  return useCallback(
    (path: string, init?: RequestInit) => window.fetch(`${apiBase}${path}`, init),
    []
  );
}

const standaloneBindings: PortalPluginHostBindings = {
  useProjectContext: () => ({
    project: DEMO_PROJECT,
    org: undefined,
    isLoading: false,
    error: null,
  }),
  usePluginFetch: useStandalonePluginFetch,
  // Not exercised by ChatDock; a static empty result satisfies the contract.
  useResourceWatch: () => ({ lastEvent: null, isConnected: false, error: null }),
};

function StandaloneHostProvider({ children }: { children: React.ReactNode }) {
  return <PortalPluginHostProvider bindings={standaloneBindings}>{children}</PortalPluginHostProvider>;
}

const queryClient = new QueryClient();

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <StandaloneHostProvider>
        <div
          style={{
            maxWidth: 960,
            margin: '2rem auto',
            padding: '0 1rem',
            fontFamily: 'system-ui',
          }}
        >
          <p style={{ opacity: 0.6 }}>
            Standalone preview — the portal loads this plugin via
            <code> /plugin-manifest.json</code> and <code>/remoteEntry.js</code>, wrapping{' '}
            <code>ChatDock</code> in its own <code>PortalPluginHostProvider</code>, not this page.
            This harness points at <code>{apiBase}</code> — run a local{' '}
            <code>cmd/assistant-apiserver</code> there to see live data; otherwise turns will fail,
            which is expected.
          </p>
          <MemoryRouter initialEntries={['/project/demo-project']}>
            <ChatDock />
          </MemoryRouter>
        </div>
      </StandaloneHostProvider>
    </QueryClientProvider>
  </StrictMode>
);
