import { AssistantWorkspace } from '@datum-cloud/datum-ui/assistant';
import { usePluginFetch, useProjectContext } from '@datum-cloud/portal-plugin-sdk';

import { ASSISTANT_CONFIG } from '../lib/assistant-config';
import { useAssistantWorkspace } from '../hooks/use-assistant-workspace';

/**
 * `ChatDock` — the module exposed as `assistant.miloapis.com/ChatDock`,
 * `$codeRef`'d by the `portal.dock/project` extension in
 * `public/plugin-manifest.json` (id `assistant-chat`, title "Patch AI").
 * cloud-portal's project-bottom-bar mounts this as one of its dock widgets;
 * it owns its own chat state end-to-end, using only the assistant's own
 * aggregated apiserver (never talking to a model vendor directly).
 *
 * Renders the shared, props-driven `@datum-cloud/datum-ui/assistant`
 * `AssistantWorkspace` — the same presentational shell cloud-portal's real
 * "Patch" assistant uses (left history rail, "Hey there" empty state,
 * suggestion chips, rich Tiptap composer) — fed entirely by
 * `useAssistantWorkspace`, which translates this plugin's own SSE envelope
 * (`src/lib/sse.ts` via `src/lib/api.ts`'s `sendMessage`) into the
 * `UIMessage[]` shape the workspace expects.
 *
 * `AssistantWorkspace` fills its container and has no opinion about
 * open/closed state. Nor does this component: cloud-portal's
 * project-bottom-bar (host) already owns opening/closing the panel this gets
 * mounted into (its own toolbar button + slide animation, via `Activity`
 * visible/hidden) — an internal open/closed toggle here would double up with
 * that and force a second click through a redundant collapsed state before
 * the workspace itself ever renders. This component only ever renders the
 * workspace.
 *
 * Reads its host wiring (fetch implementation + active project) from
 * `@datum-cloud/portal-plugin-sdk`'s `usePluginFetch()`/`useProjectContext()`
 * rather than props, since a Module Federation `$codeRef` is mounted
 * generically by the host with no way to pass props in — the host wraps this
 * tree in a `PortalPluginHostProvider` instead (see `src/main.tsx` for the
 * standalone-preview example).
 */
export default function ChatDock() {
  const { project } = useProjectContext();
  const pluginFetch = usePluginFetch();
  const projectName = project?.name ?? '';
  const workspace = useAssistantWorkspace({ pluginFetch, projectName });

  return (
    <div
      data-testid="assistant-dock"
      aria-label="Patch AI chat"
      style={{ position: 'relative', height: '100%', width: '100%' }}
    >
      <AssistantWorkspace
        config={ASSISTANT_CONFIG}
        title={workspace.title}
        messages={workspace.messages}
        status={workspace.status}
        error={workspace.error}
        isReady={workspace.isReady}
        chatList={workspace.chatList}
        currentChatId={workspace.currentChatId}
        sidebarHeader={
          project ? (
            <div style={{ minWidth: 0 }}>
              <p style={{ opacity: 0.5, fontSize: '0.7rem' }}>Project</p>
              <p style={{ fontSize: '0.75rem', fontWeight: 500 }}>
                {project.displayName ?? project.name}
              </p>
            </div>
          ) : undefined
        }
        editor={workspace.editor}
        htmlByUserMsgIndex={workspace.htmlByUserMsgIndex}
        bottomRef={workspace.bottomRef}
        containerRef={workspace.containerRef}
        userScrolledUpRef={workspace.userScrolledUpRef}
        onSend={workspace.onSend}
        onStop={workspace.onStop}
        onRetry={workspace.onRetry}
        onNewChat={workspace.onNewChat}
        onLoadChat={workspace.onLoadChat}
        onDeleteChat={workspace.onDeleteChat}
        onSuggestion={workspace.onSuggestion}
        modelId={workspace.modelId}
        effortId={workspace.effortId}
        onModelChange={workspace.onModelChange}
        onEffortChange={workspace.onEffortChange}
        micSupported={workspace.micSupported}
        micListening={workspace.micListening}
        micFrequencyData={workspace.micFrequencyData}
        onMicToggle={workspace.onMicToggle}
        historyOpen={workspace.historyOpen}
        onToggleHistory={workspace.onToggleHistory}
      />
      {!project && (
        <p role="status" style={{ position: 'absolute', bottom: 0, left: 0, right: 0, opacity: 0.7 }}>
          No project context supplied — running with an empty project scope.
        </p>
      )}
    </div>
  );
}
