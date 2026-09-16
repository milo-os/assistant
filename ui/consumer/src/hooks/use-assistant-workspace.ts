import type { ChatSummary, EffortId } from '@datum-cloud/datum-ui/assistant';
import { cn } from '@datum-cloud/datum-ui/utils';
import type { PluginFetch } from '@datum-cloud/portal-plugin-sdk';
import Placeholder from '@tiptap/extension-placeholder';
import { useEditor } from '@tiptap/react';
import StarterKit from '@tiptap/starter-kit';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import type { DynamicToolUIPart, TextUIPart, UIMessage, UIMessagePart, UIDataTypes, UITools } from 'ai';
import type { MouseEvent as ReactMouseEvent } from 'react';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import {
  deleteConversation,
  getConversationMessages,
  listConversations,
  sendMessage,
  setConversationArchived,
  type Conversation,
  type StoredMessage,
} from '../lib/api';
import { useSpeechInput } from './use-speech-input';

type Parts = UIMessagePart<UIDataTypes, UITools>[];

/**
 * `AssistantWorkspaceProps.status` values this hook produces. `submitted` is
 * used while a past conversation's transcript loads — datum-ui renders it as
 * the typing indicator and treats the workspace as not ready (no sending).
 */
export type AssistantStatus = 'ready' | 'submitted' | 'streaming' | 'error';

// The model/effort picker is hidden (`modelSelector: false` in
// `assistant-config.ts`), so these are inert placeholders required only to
// satisfy `AssistantWorkspaceProps` — never sent to the apiserver.
const INERT_MODEL_ID = '';
const INERT_EFFORT_ID: EffortId = 'high';

const NEW_CHAT_TITLE = 'New chat';

function newId(): string {
  return typeof crypto !== 'undefined' && 'randomUUID' in crypto
    ? crypto.randomUUID()
    : `${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

function escapeHtml(text: string): string {
  return text
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;');
}

/** Plain text → the paragraph HTML the Tiptap composer would have produced. */
function plainTextToHtml(text: string): string {
  return text
    .split('\n')
    .map((line) => `<p>${escapeHtml(line)}</p>`)
    .join('');
}

function textOf(msg: UIMessage): string {
  return msg.parts.find((p): p is TextUIPart => p.type === 'text')?.text ?? '';
}

function toError(err: unknown, fallback: string): Error {
  return err instanceof Error ? err : new Error(fallback);
}

function deriveTitle(messages: UIMessage[]): string {
  const text = textOf(messages.find((m) => m.role === 'user') ?? { id: '', role: 'user', parts: [] });
  return text.length > 42 ? text.slice(0, 42) + '…' : text || NEW_CHAT_TITLE;
}

function toChatSummary(conversation: Conversation): ChatSummary {
  const { metadata, spec, status } = conversation;
  return {
    id: metadata.name,
    title: status?.name || status?.title || NEW_CHAT_TITLE,
    updatedAt: Date.parse(status?.lastActiveAt ?? metadata.creationTimestamp ?? '') || 0,
    archived: spec?.archived ?? false,
    // The list endpoint carries no transcripts; they're fetched on open.
    messages: [],
  };
}

/**
 * Stored transcript → `UIMessage[]`.
 *
 * `summary` rows are the server's compaction of older turns that were folded
 * out of the transcript. They are rendered as an assistant message prefixed
 * with an "Earlier conversation (summarized)" heading rather than dropped or
 * shown as a user turn: they are model-written context, so the assistant
 * bubble (markdown-rendered) is the honest attribution, the heading makes it
 * clear this isn't a reply to the preceding message, and keeping them out of
 * the `user` role keeps `htmlByUserMsgIndex` aligned with real user turns.
 * `system` rows are internal and never shown.
 */
function toUIMessages(stored: StoredMessage[]): UIMessage[] {
  const out: UIMessage[] = [];
  for (const m of stored) {
    const id = `seq-${m.seq}`;
    switch (m.role) {
      case 'user':
      case 'assistant':
        out.push({ id, role: m.role, parts: [{ type: 'text', text: m.content, state: 'done' }] });
        break;
      case 'summary':
        out.push({
          id,
          role: 'assistant',
          parts: [
            {
              type: 'text',
              text: `**Earlier conversation (summarized)**\n\n${m.content}`,
              state: 'done',
            },
          ],
        });
        break;
    }
  }
  return out;
}

/**
 * Drives the shared `@datum-cloud/datum-ui/assistant` `AssistantWorkspace`
 * from this plugin's own transport (`src/lib/api.ts`'s `sendMessage` async
 * generator over the apiserver's `sendmessage` subresource — see
 * `src/lib/sse.ts` for the wire event vocabulary), rather than the Vercel AI
 * SDK's `useChat`/`DefaultChatTransport` that cloud-portal's old
 * `use-chat-logic.ts` used.
 *
 * Chat history is server-backed: the apiserver's Conversation list is the
 * single source of truth (fetched through the host's shared react-query
 * client), transcripts are fetched when a chat is opened, and archive/delete
 * go straight to the server with optimistic list updates. The chat id is the
 * Conversation's resource name — a fresh client-generated one for a new chat,
 * which the server creates on the first `sendmessage`.
 *
 * Tool-activity modeling: `tool_start`/`tool_finish` events become
 * `DynamicToolUIPart`s (`type: 'dynamic-tool'`) rather than the strongly-typed
 * per-tool-name `ToolUIPart`s — `DynamicToolUIPart` carries `toolName` as a
 * plain field precisely for tool sets not known at compile time, which
 * matches this backend (the apiserver can call any tool the a2a runner
 * exposes; the client has no static tool registry). `tool_start` maps to
 * `state: 'input-available'` (we only get a name + optional summary, no
 * structured input) and `tool_finish` maps to `output-available`/
 * `output-error`.
 */
/** What this hook needs from the host, sourced by the caller from the SDK's
 * `usePluginFetch()`/`useProjectContext()` hooks (see `chat-dock.tsx`). */
export interface AssistantWorkspaceHostContext {
  pluginFetch: PluginFetch;
  projectName: string;
}

export function useAssistantWorkspace(ctx: AssistantWorkspaceHostContext) {
  const { pluginFetch, projectName } = ctx;
  const bottomRef = useRef<HTMLDivElement>(null);

  // ── Messages / status ─────────────────────────────────────────────────────
  const [messages, setMessages] = useState<UIMessage[]>([]);
  const messagesRef = useRef<UIMessage[]>(messages);
  useEffect(() => {
    messagesRef.current = messages;
  }, [messages]);

  const [status, setStatus] = useState<AssistantStatus>('ready');
  const statusRef = useRef(status);
  statusRef.current = status;
  const [error, setError] = useState<Error | undefined>();

  const isReady = status === 'ready' || status === 'error';

  const abortRef = useRef<AbortController | null>(null);

  // Tiptap HTML per user message, by position in the user-message array.
  const htmlByUserMsgIndex = useRef<string[]>([]);

  const [currentChatId, setCurrentChatId] = useState<string>(() => newId());
  const currentChatIdRef = useRef(currentChatId);

  /**
   * Bumped on every chat switch (new chat / open chat). An in-flight turn or
   * transcript load captures it and drops its state updates once it no
   * longer matches, so a stale stream or slow fetch can't write into the
   * chat the user has since moved to.
   */
  const sessionRef = useRef(0);

  /** Set when opening a chat's transcript failed, so Retry reloads it. */
  const failedLoadChatIdRef = useRef<string | null>(null);

  // ── Chat history (server-backed) ──────────────────────────────────────────
  const queryClient = useQueryClient();
  const conversationsKey = useMemo(
    () => ['assistant.miloapis.com', 'conversations', projectName] as const,
    [projectName]
  );

  const conversationsQuery = useQuery({
    queryKey: conversationsKey,
    enabled: !!projectName,
    queryFn: async (): Promise<ChatSummary[]> => {
      // The server lists active and archived conversations as disjoint sets;
      // the history panel wants both and filters on `archived` itself.
      const [active, archived] = await Promise.all([
        listConversations(pluginFetch, projectName),
        listConversations(pluginFetch, projectName, { archived: true }),
      ]);
      const byId = new Map<string, ChatSummary>();
      for (const c of [...active, ...archived]) {
        const summary = toChatSummary(c);
        if (!byId.has(summary.id)) byId.set(summary.id, summary);
      }
      return [...byId.values()].sort((a, b) => b.updatedAt - a.updatedAt);
    },
  });
  const chatList = useMemo(() => conversationsQuery.data ?? [], [conversationsQuery.data]);

  useEffect(() => {
    if (conversationsQuery.error) {
      setError(toError(conversationsQuery.error, 'Could not load chat history.'));
    }
  }, [conversationsQuery.error]);

  const refreshChatList = useCallback(() => {
    void queryClient.invalidateQueries({ queryKey: conversationsKey });
  }, [queryClient, conversationsKey]);

  /**
   * Applies `update` to the cached list immediately, runs `action` against
   * the server, and restores the pre-update list (surfacing the error) if it
   * fails. Either way the list is refetched afterwards to reconcile.
   */
  const mutateChatList = useCallback(
    async (
      update: (list: ChatSummary[]) => ChatSummary[],
      action: () => Promise<unknown>,
      failureMessage: string
    ) => {
      await queryClient.cancelQueries({ queryKey: conversationsKey });
      const previous = queryClient.getQueryData<ChatSummary[]>(conversationsKey);
      queryClient.setQueryData<ChatSummary[]>(conversationsKey, (list) => update(list ?? []));
      try {
        await action();
      } catch (err) {
        queryClient.setQueryData(conversationsKey, previous);
        setError(toError(err, failureMessage));
      } finally {
        refreshChatList();
      }
    },
    [queryClient, conversationsKey, refreshChatList]
  );

  /**
   * Runs one turn: appends a user + in-flight assistant message, then drives
   * `sendMessage`'s SSE stream into the assistant message's `parts`.
   * `baseMessages` lets `onRetry` supply an already-truncated message list
   * without racing `messagesRef` (which only reflects the last *committed*
   * render, and a retry's `setMessages` truncation hasn't committed yet when
   * this is called).
   */
  const runTurn = useCallback(
    async (text: string, opts?: { html?: string; baseMessages?: UIMessage[] }) => {
      if (statusRef.current === 'streaming' || statusRef.current === 'submitted') return;
      const trimmed = text.trim();
      if (!trimmed) return;

      const session = sessionRef.current;
      const isCurrent = () => session === sessionRef.current;

      setError(undefined);
      htmlByUserMsgIndex.current.push(opts?.html ?? plainTextToHtml(trimmed));

      const priorMessages = opts?.baseMessages ?? messagesRef.current;
      const userMsg: UIMessage = {
        id: newId(),
        role: 'user',
        parts: [{ type: 'text', text: trimmed }],
      };
      const assistantId = newId();

      setMessages([...priorMessages, userMsg, { id: assistantId, role: 'assistant', parts: [] }]);
      setStatus('streaming');

      const controller = new AbortController();
      abortRef.current = controller;

      let lastToolCallId: string | undefined;

      const applyParts = (mutate: (parts: Parts) => Parts) => {
        if (!isCurrent()) return;
        setMessages((prev) =>
          prev.map((m) => (m.id === assistantId ? { ...m, parts: mutate(m.parts) } : m))
        );
      };

      try {
        for await (const event of sendMessage(
          projectName,
          currentChatIdRef.current,
          { text: trimmed },
          controller.signal
        )) {
          switch (event.type) {
            case 'text_delta':
              applyParts((parts) => {
                const last = parts[parts.length - 1];
                if (last?.type === 'text') {
                  return [
                    ...parts.slice(0, -1),
                    { ...last, text: last.text + event.text, state: 'streaming' },
                  ];
                }
                const part: TextUIPart = { type: 'text', text: event.text, state: 'streaming' };
                return [...parts, part];
              });
              break;

            case 'tool_start': {
              const toolCallId = event.id ?? newId();
              lastToolCallId = toolCallId;
              applyParts((parts) => {
                const part: DynamicToolUIPart = {
                  type: 'dynamic-tool',
                  toolName: event.name,
                  toolCallId,
                  state: 'input-available',
                  input: event.summary ? { summary: event.summary } : {},
                };
                return [...parts, part];
              });
              break;
            }

            case 'tool_finish': {
              // The backend omits `id` when the provider assigned no
              // tool-call id; fall back to the most recently started tool.
              const toolCallId = event.id ?? lastToolCallId;
              applyParts((parts) =>
                parts.map((p) => {
                  if (p.type !== 'dynamic-tool' || p.toolCallId !== toolCallId) return p;
                  // Widen the (state-narrowed) input back to `unknown` and
                  // carry it forward explicitly — the union's other branches
                  // make `input` optional, so a plain `{ ...p, state: … }`
                  // spread can't satisfy `DynamicToolUIPart`'s per-state
                  // required fields.
                  const input: unknown = p.input;
                  if (event.ok) {
                    const done: DynamicToolUIPart = {
                      type: 'dynamic-tool',
                      toolName: p.toolName,
                      toolCallId: p.toolCallId,
                      input,
                      state: 'output-available',
                      output: { ok: true, elapsedMs: event.elapsedMs },
                    };
                    return done;
                  }
                  const failed: DynamicToolUIPart = {
                    type: 'dynamic-tool',
                    toolName: p.toolName,
                    toolCallId: p.toolCallId,
                    input,
                    state: 'output-error',
                    errorText: `${event.name} failed`,
                  };
                  return failed;
                })
              );
              break;
            }

            case 'done': {
              applyParts((parts) => {
                let next = parts.map((p) =>
                  p.type === 'text' ? ({ ...p, state: 'done' } as TextUIPart) : p
                );
                // The server's authoritative final text should already match
                // what the deltas accumulated; only backfill if we somehow
                // have none (e.g. a turn with no text_delta events).
                if (event.text && !next.some((p) => p.type === 'text')) {
                  next = [...next, { type: 'text', text: event.text, state: 'done' } as TextUIPart];
                }
                return next;
              });
              if (!isCurrent()) break;
              if (event.state === 'failed') {
                setError(new Error(event.error || 'The request failed.'));
                setStatus('error');
              } else {
                setStatus('ready');
              }
              break;
            }
          }
        }
      } catch (err) {
        if (isCurrent()) {
          if (controller.signal.aborted) {
            setStatus('ready');
          } else {
            setError(toError(err, 'The request failed.'));
            setStatus('error');
          }
        }
      } finally {
        if (abortRef.current === controller) abortRef.current = null;
        // Even if the user has moved on, the turn may have created the
        // conversation, renamed it, or unarchived it — pick that up.
        refreshChatList();
      }
    },
    [pluginFetch, projectName, refreshChatList]
  );

  const stop = useCallback(() => {
    abortRef.current?.abort();
  }, []);

  // ── Chat switching ────────────────────────────────────────────────────────
  /** Cancels whatever the current chat is doing and points the hook at `chatId`. */
  const switchTo = useCallback((chatId: string) => {
    sessionRef.current += 1;
    abortRef.current?.abort();
    abortRef.current = null;
    failedLoadChatIdRef.current = null;
    currentChatIdRef.current = chatId;
    setCurrentChatId(chatId);
    htmlByUserMsgIndex.current = [];
    setMessages([]);
    setError(undefined);
  }, []);

  const startNewChat = useCallback(() => {
    switchTo(newId());
    setStatus('ready');
  }, [switchTo]);

  const loadChat = useCallback(
    async (chatId: string) => {
      switchTo(chatId);
      const session = sessionRef.current;
      setStatus('submitted');
      try {
        const loaded = toUIMessages(await getConversationMessages(pluginFetch, projectName, chatId));
        if (session !== sessionRef.current) return;
        htmlByUserMsgIndex.current = loaded
          .filter((m) => m.role === 'user')
          .map((m) => plainTextToHtml(textOf(m)));
        setMessages(loaded);
        setStatus('ready');
        setTimeout(() => bottomRef.current?.scrollIntoView({ behavior: 'instant' }), 50);
      } catch (err) {
        if (session !== sessionRef.current) return;
        failedLoadChatIdRef.current = chatId;
        setError(toError(err, 'Could not load this conversation.'));
        setStatus('error');
      }
    },
    [pluginFetch, projectName, switchTo]
  );

  const onLoadChat = useCallback(
    (chat: ChatSummary) => {
      // Re-opening the chat that's mid-reply would abort the reply; ignore it.
      if (chat.id === currentChatIdRef.current && statusRef.current === 'streaming') return;
      void loadChat(chat.id);
    },
    [loadChat]
  );

  const setArchived = useCallback(
    (e: ReactMouseEvent, chatId: string, archived: boolean) => {
      e.stopPropagation();
      if (!projectName) return;
      if (archived && chatId === currentChatIdRef.current) startNewChat();
      void mutateChatList(
        (list) => list.map((c) => (c.id === chatId ? { ...c, archived } : c)),
        () => setConversationArchived(pluginFetch, projectName, chatId, archived),
        archived ? 'Could not archive this chat.' : 'Could not restore this chat.'
      );
    },
    [pluginFetch, projectName, startNewChat, mutateChatList]
  );

  const onArchiveChat = useCallback(
    (e: ReactMouseEvent, chatId: string) => setArchived(e, chatId, true),
    [setArchived]
  );

  const onUnarchiveChat = useCallback(
    (e: ReactMouseEvent, chatId: string) => setArchived(e, chatId, false),
    [setArchived]
  );

  const onDeleteChat = useCallback(
    (e: ReactMouseEvent, chatId: string) => {
      e.stopPropagation();
      if (!projectName) return;
      if (chatId === currentChatIdRef.current) startNewChat();
      void mutateChatList(
        (list) => list.filter((c) => c.id !== chatId),
        () => deleteConversation(pluginFetch, projectName, chatId),
        'Could not delete this chat.'
      );
    },
    [pluginFetch, projectName, startNewChat, mutateChatList]
  );

  // Chat ids are project-scoped conversation names; a project switch starts over.
  const isFirstProjectRef = useRef(true);
  useEffect(() => {
    if (isFirstProjectRef.current) {
      isFirstProjectRef.current = false;
      return;
    }
    startNewChat();
  }, [projectName, startNewChat]);

  // ── Editor ────────────────────────────────────────────────────────────────
  // `runTurn`/`isReady` are read through refs inside `handleKeyDown` so the
  // closure captured at editor-creation time always sees the latest values.
  const runTurnRef = useRef(runTurn);
  runTurnRef.current = runTurn;
  const isReadyRef = useRef(isReady);
  isReadyRef.current = isReady;

  const editor = useEditor({
    extensions: [
      StarterKit.configure({
        heading: false,
        codeBlock: false,
        code: false,
        blockquote: false,
        bulletList: false,
        orderedList: false,
        listItem: false,
        horizontalRule: false,
        // StarterKit bundles Link (autolink + openOnClick); in a plain-text
        // prompt a pasted email becomes a clickable mailto. Disable it.
        link: false,
      }),
      Placeholder.configure({ placeholder: 'Ask Patch anything…' }),
    ],
    editorProps: {
      attributes: {
        class: cn(
          'prose prose-sm dark:prose-invert max-w-none',
          'px-1 py-1 text-sm focus:outline-none',
          '[&_p]:my-0.5'
        ),
      },
      handleKeyDown: (view, event) => {
        if (event.key === 'Enter' && !event.shiftKey) {
          event.preventDefault();
          const text = view.state.doc.textContent.trim();
          if (text && isReadyRef.current) {
            const html = editor?.getHTML() ?? plainTextToHtml(text);
            void runTurnRef.current(text, { html });
            const { state } = view;
            view.dispatch(
              state.tr.replaceWith(0, state.doc.content.size, state.schema.nodes.paragraph.create())
            );
          }
          return true;
        }
        return false;
      },
    },
  });

  const speech = useSpeechInput(editor);

  const onSend = useCallback(() => {
    if (!editor || !isReadyRef.current) return;
    const text = editor.getText().trim();
    if (!text) return;
    const html = editor.getHTML();
    void runTurn(text, { html });
    editor.commands.clearContent();
    editor.commands.focus();
    setTimeout(() => bottomRef.current?.scrollIntoView({ behavior: 'smooth' }), 50);
  }, [editor, runTurn]);

  const onSuggestion = useCallback(
    (suggestion: string) => {
      if (!isReadyRef.current) return;
      void runTurn(suggestion, { html: plainTextToHtml(suggestion) });
    },
    [runTurn]
  );

  const onRetry = useCallback(() => {
    // A failed transcript load retries the load, not a turn.
    const failedLoad = failedLoadChatIdRef.current;
    if (failedLoad && failedLoad === currentChatIdRef.current) {
      void loadChat(failedLoad);
      return;
    }

    const msgs = messagesRef.current;
    const lastUserIdx = msgs.map((m) => m.role).lastIndexOf('user');
    if (lastUserIdx === -1) return;
    const text = textOf(msgs[lastUserIdx]);
    if (!text) return;

    const truncated = msgs.slice(0, lastUserIdx);
    const retainedHtml = htmlByUserMsgIndex.current.slice(0, -1);
    setMessages(truncated);
    htmlByUserMsgIndex.current = retainedHtml;
    setError(undefined);
    void runTurn(text, { baseMessages: truncated });
  }, [loadChat, runTurn]);

  // ── Auto-scroll (mirrors old assistant-workspace's MutationObserver) ─────
  const userScrolledUpRef = useRef(false);
  const scrollRaf = useRef(0);
  const containerRef = useCallback((node: HTMLDivElement | null) => {
    if (!node) return;
    const observer = new MutationObserver(() => {
      if (userScrolledUpRef.current) return;
      cancelAnimationFrame(scrollRaf.current);
      scrollRaf.current = requestAnimationFrame(() => {
        node.scrollTo({ top: node.scrollHeight, behavior: 'smooth' });
      });
    });
    observer.observe(node, { childList: true, subtree: true, characterData: true });
    return () => {
      observer.disconnect();
      cancelAnimationFrame(scrollRaf.current);
    };
  }, []);

  const [historyOpen, setHistoryOpen] = useState(false);

  const title = useMemo(() => {
    const serverTitle = chatList.find((c) => c.id === currentChatId)?.title;
    // A brand-new conversation isn't listed (or titled) until the server has
    // it, so fall back to the first user message meanwhile.
    if (serverTitle && serverTitle !== NEW_CHAT_TITLE) return serverTitle;
    return messages.length > 0 ? deriveTitle(messages) : NEW_CHAT_TITLE;
  }, [chatList, currentChatId, messages]);

  return {
    title,
    messages,
    status,
    error,
    isReady,
    chatList,
    currentChatId,
    editor,
    htmlByUserMsgIndex,
    bottomRef,
    containerRef,
    userScrolledUpRef,
    onSend,
    onStop: stop,
    onRetry,
    onNewChat: startNewChat,
    onLoadChat,
    onArchiveChat,
    onUnarchiveChat,
    onDeleteChat,
    onSuggestion,
    modelId: INERT_MODEL_ID,
    effortId: INERT_EFFORT_ID,
    onModelChange: () => {},
    onEffortChange: () => {},
    micSupported: speech.isSupported,
    micListening: speech.isListening,
    micFrequencyData: speech.frequencyData,
    onMicToggle: speech.isListening ? speech.stopListening : speech.startListening,
    historyOpen,
    onToggleHistory: () => setHistoryOpen((o) => !o),
  };
}
