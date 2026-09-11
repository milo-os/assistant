import { federation } from '@module-federation/vite';
import react from '@vitejs/plugin-react';
import { defineConfig } from 'vite';

// Assistant ("Patch") Portal Plugin — a Module Federation remote loaded by
// the cloud-portal host at runtime. Structural template: compute's
// ui/consumer/vite.config.ts (itself modeled on
// examples/sample-plugin/ in the cloud-portal repo — see its README.md and
// docs/enhancements/portal-plugin-system.md there).
//
// The host (cloud-portal) loads this remote via @module-federation/runtime and
// provides react / react-dom / react-router / @tanstack/react-query as shared
// singletons, so the plugin renders with the host's exact React, router, and
// query client. `shared` below marks those as singletons with the host's
// version so the host copy always wins and React is never duplicated (two
// React instances break hooks).
//
// Assets are fetched server-side by the portal's asset proxy and served under
// /api/plugins/<slug>/…, so plain http://localhost during dev is fine and the
// browser never contacts this origin directly. MF's automatic publicPath makes
// federated chunks resolve relative to remoteEntry.js, which is what lets them
// load correctly through that same-origin proxy prefix.
export default defineConfig({
  server: {
    port: 7779,
    strictPort: true,
    // Allow cross-origin fetches of the manifest/remote during Tier 0/standalone.
    cors: true,
  },
  preview: {
    port: 7779,
    strictPort: true,
    cors: true,
  },
  build: {
    target: 'esnext',
    // Keep the plugin readable when inspecting the built bundle.
    minify: false,
  },
  plugins: [
    react(),
    federation({
      // MUST equal the manifest `name` — the host keys the remote by this id.
      // Matches the apiserver's API group (assistant.miloapis.com).
      name: 'assistant.miloapis.com',
      // The manifest's `remoteEntry` field points the host at this filename,
      // requested through the asset proxy as /api/plugins/assistant/remoteEntry.js.
      filename: 'remoteEntry.js',
      manifest: true,
      // Exposed keys map 1:1 to the manifest's `exposedModules` keys / $codeRefs.
      // The host loads e.g. loadRemote('assistant.miloapis.com/ChatDock').
      exposes: {
        './ChatDock': './src/widgets/chat-dock.tsx',
      },
      // Host-pinned singletons. requiredVersion:false, matching the host's own
      // federation-host.ts hostShared() — the host copy always wins,
      // regardless of what version this plugin was built against. A strict
      // requiredVersion here would re-break on the host's next major bump
      // (this happened once already: cloud-portal moved to react-router v8
      // while this stayed pinned to ^7.0.0, which made Module Federation
      // refuse to bridge the shared modules at all — a hard runtime crash,
      // not just a warning). singleton:true still guarantees one instance —
      // the host provides all of these, so plugin queries share the host's
      // QueryClient cache.
      shared: {
        react: { singleton: true, requiredVersion: false },
        'react-dom': { singleton: true, requiredVersion: false },
        'react-router': { singleton: true, requiredVersion: false },
        '@tanstack/react-query': { singleton: true, requiredVersion: false },
        // Must be shared, not bundled: useProjectContext()/usePluginFetch()
        // read a React Context defined inside this module, and that Context
        // is only the host's PortalPluginHostProvider's Context if every
        // consumer resolves the host's exact module instance instead of this
        // plugin's own copy. requiredVersion:false matches the host's own
        // federation-host.ts — SDK compatibility is checked separately via
        // the manifest's sdk.range.
        '@datum-cloud/portal-plugin-sdk': { singleton: true, requiredVersion: false },
        // Curated datum-ui subset shared by the host (see the host's
        // federation-host.ts DATUM_UI_SHARED). requiredVersion:false — the
        // host's copy always wins, which is what keeps styling identical to
        // built-in pages; the local install is types + standalone fallback.
        '@datum-cloud/datum-ui/badge': { singleton: true, requiredVersion: false },
        '@datum-cloud/datum-ui/button': { singleton: true, requiredVersion: false },
        '@datum-cloud/datum-ui/card': { singleton: true, requiredVersion: false },
        '@datum-cloud/datum-ui/icons': { singleton: true, requiredVersion: false },
        // Do not share `logs`: MF colocates lucide-react / date-fns into the
        // logs loadShare chunk, so a host-provided logs module replaces those
        // exports and crashes unrelated pages. (Kept in lockstep with compute.)
        '@datum-cloud/datum-ui/separator': { singleton: true, requiredVersion: false },
        '@datum-cloud/datum-ui/skeleton': { singleton: true, requiredVersion: false },
        '@datum-cloud/datum-ui/table': { singleton: true, requiredVersion: false },
      },
    }),
  ],
});
