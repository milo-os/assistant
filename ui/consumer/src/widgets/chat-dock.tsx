import {
  AssistantConfigProvider,
  AssistantWorkspace,
  Conversation,
  PromptCard,
} from '@datum-cloud/datum-ui/assistant';
import { usePluginFetch, useProjectContext } from '@datum-cloud/portal-plugin-sdk';
import { useAssistantWorkspace, type AssistantSurface } from '@datum-cloud/assistant-chat-kit';
import { useEffect, useRef, type CSSProperties } from 'react';

import { ASSISTANT_CONFIG } from '../lib/assistant-config';

/**
 * Props cloud-portal mounts every `portal.dock/project` widget with (its
 * `DockWidgetProps`). Optional, so the standalone preview can mount this bare.
 */
interface ChatDockProps {
  /** Closes the dock panel. */
  onClose?: () => void;
  /**
   * Set when the host opens the dock to ask something. A new `id` means a
   * new request, even with the same `text`.
   */
  prompt?: { id: string; text: string };
  /**
   * Open this existing conversation. A new value means "switch to it"; the
   * same value again is a no-op.
   */
  chatId?: string;
  /** Called when the current conversation changes, once it is saved server side (has messages, no reply streaming). */
  onChatChange?: (chatId: string) => void;
  /**
   * Where the host placed it: `dock` (default) is the tall, narrow side
   * panel, `inline` a wide area in the page body.
   */
  placement?: 'dock' | 'inline';
  /**
   * The portal surface the host embedded this in (e.g. cloud-portal's
   * project home page passes `{ name: 'home-first-run', network }`). Sent
   * with every turn so Patch can tailor its replies; a change applies to the
   * next turn without resetting the chat. Unset sends no surface at all.
   */
  surface?: AssistantSurface;
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
 * The dock is a tall, narrow column, so by default the workspace renders in
 * its vertical orientation (top bar + history drawer). The manifest declares
 * `handlesClose`, so the host drops the close button it would otherwise
 * overlay on the panel's corner and mounts this with an `onClose` prop
 * instead; the workspace renders that in its own header.
 *
 * With `placement="inline"` the host has put it in the page body and draws
 * the only header itself (title, "open in side panel", and a close control
 * wired to the same `onClose`). There the workspace shell is not rendered at
 * all: no rail, history, title bar or close button. Only datum-ui's
 * `Conversation` (the single scroll area, in its compact density) with the
 * `PromptCard` pinned under it, inside `AssistantConfigProvider`, so it reads
 * as a section of the page.
 * Before the first message it shows a one-line placeholder above the
 * composer instead of the greeting and suggestion chips. `onClose` is unused
 * there.
 *
 * Reads its host wiring (fetch implementation + active project) from
 * `@datum-cloud/portal-plugin-sdk`'s `usePluginFetch()`/`useProjectContext()`
 * — the host wraps this tree in a `PortalPluginHostProvider` (see
 * `src/main.tsx` for the standalone-preview example). Its props are the
 * dock's `onClose` plus the optional `prompt`, `chatId`, `onChatChange`,
 * `placement` and `surface`.
 *
 * `prompt` lets the host open the dock with a question already asked (e.g.
 * a "Ask Patch" button elsewhere in the portal). Each new `prompt.id` starts
 * a fresh chat and sends `prompt.text` as its first message, once. If the
 * workspace is mid-turn or the project hasn't resolved yet, the prompt waits
 * until it can be sent rather than aborting the turn or being dropped. With
 * no `prompt` the dock behaves as a plain, empty chat.
 *
 * `chatId` and `onChatChange` let the host move one conversation between
 * placements (e.g. from the page body into the dock). `onChatChange` reports
 * the current chat id once it is saved on the server (it has messages and no
 * reply is streaming), and again whenever it changes to another saved chat;
 * handing that id to
 * another instance as `chatId` opens it there, once readiness allows, the
 * same way the prompt waits. A `prompt` that has not been acted on yet wins
 * over a `chatId` arriving with it. Only the persisted transcript is loaded,
 * so a turn still streaming in the other instance shows up to its last
 * saved message.
 */
export default function ChatDock({
  onClose,
  prompt,
  chatId,
  onChatChange,
  placement = 'dock',
  surface,
}: ChatDockProps) {
  const { project } = useProjectContext();
  const pluginFetch = usePluginFetch();
  const projectName = project?.name ?? '';
  const workspace = useAssistantWorkspace({ pluginFetch, projectName, surface });

  // Host prompts run in two commits. `onNewChat` points the hook's chat-id
  // ref at a fresh id synchronously, but `onSuggestion` reads the prior
  // messages and readiness from refs that only catch up on the next commit;
  // sending in the same tick would prepend the old chat's transcript. So the
  // first pass starts the chat and parks the text, and the second pass (once
  // `currentChatId` has moved off the old id) sends it. Both refs survive
  // StrictMode's double effects, so a prompt id is acted on at most once.
  //
  // A host `chatId` is acted on once per distinct value, tracked the same
  // way, so the same id passed again never reloads the chat. Its load is
  // checked on the commit where `currentChatId` catches up: the hook's own
  // project effect starts a new chat on StrictMode's effect re-run (and on a
  // project change), which can land just after the load and replace it. A
  // load replaced that way is retried once.
  const handledPromptIdRef = useRef<string | null>(null);
  const pendingSendRef = useRef<{ text: string; fromChatId: string } | null>(null);
  const handledChatIdRef = useRef<string | null>(null);
  const pendingLoadRef = useRef<{ chatId: string; fromChatId: string; retried: boolean } | null>(null);
  const { isReady, currentChatId, chatList, onNewChat, onLoadChat, onSuggestion } = workspace;
  const messageCount = workspace.messages.length;

  useEffect(() => {
    // A prompt not yet sent wins over a `chatId` arriving with it: drop the
    // id rather than open it over the prompt's new chat later.
    const promptOpen = (prompt && prompt.id !== handledPromptIdRef.current) || pendingSendRef.current;
    if (chatId && chatId !== handledChatIdRef.current && promptOpen) handledChatIdRef.current = chatId;

    const pending = pendingSendRef.current;
    if (pending && currentChatId !== pending.fromChatId) {
      pendingSendRef.current = null;
      // Anything other than a blank, idle chat means the user moved on
      // (e.g. opened a past chat) before this commit; don't send into it.
      if (isReady && messageCount === 0) onSuggestion(pending.text);
      return;
    }

    if (prompt && prompt.id !== handledPromptIdRef.current) {
      // Wait out an in-flight turn or transcript load (`onNewChat` would
      // abort it) and an unresolved project (a turn needs its name).
      if (!isReady || !projectName) return;
      handledPromptIdRef.current = prompt.id;
      pendingSendRef.current = { text: prompt.text, fromChatId: currentChatId };
      // The prompt's chat replaces any host chat still being checked.
      pendingLoadRef.current = null;
      onNewChat();
      return;
    }

    const load = pendingLoadRef.current;
    if (load && currentChatId !== load.fromChatId) {
      pendingLoadRef.current = null;
      // Replaced before it showed, and the host still wants it: go again.
      if (currentChatId !== load.chatId && load.chatId === chatId && !load.retried) {
        pendingLoadRef.current = { ...load, fromChatId: currentChatId, retried: true };
        onLoadChat({ id: load.chatId, title: '', updatedAt: 0, messages: [] });
      }
      return;
    }

    if (!chatId || chatId === handledChatIdRef.current) return;
    if (chatId === currentChatId) {
      handledChatIdRef.current = chatId;
      return;
    }
    // Same waits as a prompt: loading aborts a running turn, and the
    // transcript read is scoped by the project name.
    if (!isReady || !projectName) return;
    handledChatIdRef.current = chatId;
    pendingLoadRef.current = { chatId, fromChatId: currentChatId, retried: false };
    // `onLoadChat` only reads the id; the list entry, when there is one,
    // just keeps the summary honest.
    onLoadChat(chatList.find((c) => c.id === chatId) ?? { id: chatId, title: '', updatedAt: 0, messages: [] });
  }, [prompt, chatId, isReady, projectName, currentChatId, chatList, messageCount, onNewChat, onLoadChat, onSuggestion]);

  // Report each distinct chat id once, so StrictMode's double effects and
  // unrelated re-renders don't repeat it to the host. Only a saved chat is
  // reported: the assistant stores a conversation when a turn finishes with
  // an answer, so a blank chat, or one whose first reply is still streaming,
  // would 404 in another instance asked to open it.
  const savedChat = messageCount > 0 && workspace.status === 'ready';
  const reportedChatIdRef = useRef<string | null>(null);
  useEffect(() => {
    if (!onChatChange || !savedChat || currentChatId === reportedChatIdRef.current) return;
    reportedChatIdRef.current = currentChatId;
    onChatChange(currentChatId);
  }, [currentChatId, savedChat, onChatChange]);

  const noProjectNotice = !project && (
    <p role="status" style={{ position: 'absolute', bottom: 0, left: 0, right: 0, opacity: 0.7 }}>
      No project context supplied — running with an empty project scope.
    </p>
  );

  if (placement === 'inline') {
    const promptCard = (
      <PromptCard
        editor={workspace.editor}
        isReady={workspace.isReady}
        canRetry={workspace.status === 'error'}
        onSend={workspace.onSend}
        onStop={workspace.onStop}
        onRetry={workspace.onRetry}
        modelId={workspace.modelId}
        effortId={workspace.effortId}
        onModelChange={workspace.onModelChange}
        onEffortChange={workspace.onEffortChange}
        speechSupported={workspace.micSupported}
        isListening={workspace.micListening}
        frequencyData={workspace.micFrequencyData}
        onMicToggle={workspace.onMicToggle}
      />
    );
    return (
      <div data-testid="assistant-inline" aria-label="Patch AI chat" style={INLINE_ROOT_STYLE}>
        <AssistantConfigProvider config={ASSISTANT_CONFIG}>
          {workspace.messages.length > 0 ? (
            // `Conversation` owns the only scroll area and pins `footer`
            // under it, the same wiring the workspace uses. Compact density
            // (avatar beside Patch's turns, narrower column, smaller text)
            // so it reads as part of the page rather than a full chat app.
            <Conversation
              density="compact"
              messages={workspace.messages}
              status={workspace.status}
              error={workspace.error}
              htmlByUserMsgIndex={workspace.htmlByUserMsgIndex}
              containerRef={workspace.containerRef}
              bottomRef={workspace.bottomRef}
              userScrolledUpRef={workspace.userScrolledUpRef}
              footer={promptCard}
            />
          ) : (
            <>
              <div style={INLINE_EMPTY_STYLE}>
                {workspace.error ? (
                  <p role="alert" style={{ color: 'var(--destructive, #dc2626)' }}>
                    {workspace.error.message}
                  </p>
                ) : (
                  <p>Ask Patch anything about this project.</p>
                )}
              </div>
              {/* Same footer padding and width as compact `Conversation`
                  (42rem), so the composer doesn't move when the first
                  message lands. */}
              <div style={{ flexShrink: 0, padding: '0 1rem 1rem' }}>
                <div style={{ maxWidth: '42rem', margin: '0 auto', width: '100%' }}>{promptCard}</div>
              </div>
            </>
          )}
        </AssistantConfigProvider>
        {noProjectNotice}
      </div>
    );
  }

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
      {noProjectNotice}
    </div>
  );
}

/** Fills the host's fixed-height flex box; `Conversation` scrolls inside it. */
const INLINE_ROOT_STYLE: CSSProperties = {
  position: 'relative',
  display: 'flex',
  flexDirection: 'column',
  height: '100%',
  width: '100%',
  minHeight: 0,
  overflow: 'hidden',
};

/** The blank chat's one-line placeholder, filling the space above the composer. */
const INLINE_EMPTY_STYLE: CSSProperties = {
  flex: 1,
  minHeight: 0,
  overflowY: 'auto',
  display: 'flex',
  alignItems: 'flex-end',
  justifyContent: 'center',
  padding: '1rem',
  fontSize: '0.875rem',
  textAlign: 'center',
  color: 'var(--muted-foreground)',
};
