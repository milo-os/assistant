import { AssistantWorkspace } from '@datum-cloud/datum-ui/assistant';
import { usePluginFetch, useProjectContext } from '@datum-cloud/portal-plugin-sdk';
import { useAssistantWorkspace } from '@datum-cloud/assistant-chat-kit';

import { ASSISTANT_CONFIG } from '../lib/assistant-config';

/**
 * Props cloud-portal mounts every `portal.dock/project` widget with (its
 * `DockWidgetProps`). Optional, so the standalone preview can mount this bare.
 */
interface ChatDockProps {
  /** Closes the dock panel. */
  onClose?: () => void;
}

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
 * `@datum-cloud/assistant-chat-kit`'s `useAssistantWorkspace`, which
 * translates the assistant's SSE envelope into the `UIMessage[]` shape the
 * workspace expects. That package holds the conversation logic so other
 * plugins (e.g. interconnect's own assistant page) can reuse it too — this
 * component only supplies the host wiring and renders the shared UI.
 *
 * `AssistantWorkspace` fills its container and has no opinion about
 * open/closed state. Nor does this component: cloud-portal's dock panel (host)
 * already owns opening/closing the panel this gets mounted into (its own
 * header button + slide animation, via `Activity` visible/hidden) — an
 * internal open/closed toggle here would double up with that and force a
 * second click through a redundant collapsed state before the workspace
 * itself ever renders. This component only ever renders the workspace.
 *
 * The dock is a tall, narrow column, so the workspace renders in its vertical
 * orientation (top bar + history drawer). The manifest declares
 * `handlesClose`, so the host drops the close button it would otherwise
 * overlay on the panel's corner and mounts this with an `onClose` prop
 * instead; the workspace renders that in its own header.
 *
 * Reads its host wiring (fetch implementation + active project) from
 * `@datum-cloud/portal-plugin-sdk`'s `usePluginFetch()`/`useProjectContext()`
 * — the host wraps this tree in a `PortalPluginHostProvider` (see
 * `src/main.tsx` for the standalone-preview example). The only prop is the
 * dock's `onClose`.
 */
export default function ChatDock({ onClose }: ChatDockProps) {
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
        orientation="vertical"
        onClose={onClose}
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
        onArchiveChat={workspace.onArchiveChat}
        onUnarchiveChat={workspace.onUnarchiveChat}
        onDeleteChat={workspace.onDeleteChat}
        // Delete is a hard, irreversible server delete — always confirm.
        confirmDelete
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
      {/* datum-ui only renders `error` inside the conversation view; its empty
          state has no error slot. A failed transcript load or a failed
          archive/delete made from a blank chat would otherwise be silent. */}
      {workspace.error && workspace.messages.length === 0 && (
        <p
          role="alert"
          style={{ position: 'absolute', top: 0, left: 0, right: 0, padding: '0.5rem', textAlign: 'center', fontSize: '0.75rem', color: 'var(--destructive, #dc2626)' }}
        >
          {workspace.error.message}
        </p>
      )}
      {!project && (
        <p role="status" style={{ position: 'absolute', bottom: 0, left: 0, right: 0, opacity: 0.7 }}>
          No project context supplied — running with an empty project scope.
        </p>
      )}
    </div>
  );
}
