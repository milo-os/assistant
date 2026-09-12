import { sanitizeUserHtml, type ChatSummary, type EffortId } from '@datum-cloud/datum-ui/assistant';
import { cn } from '@datum-cloud/datum-ui/utils';
import Placeholder from '@tiptap/extension-placeholder';
import { useEditor } from '@tiptap/react';
import StarterKit from '@tiptap/starter-kit';
import type { DynamicToolUIPart, TextUIPart, UIMessage, UIMessagePart, UIDataTypes, UITools } from 'ai';
import type { MouseEvent as ReactMouseEvent } from 'react';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import { sendMessage } from '../lib/api';
import { deleteChat, deriveTitle, listChats, saveChat, type StoredChat } from '../lib/chat-storage';
import type { PluginFetch } from '@datum-cloud/portal-plugin-sdk';
import { useSpeechInput } from './use-speech-input';

type Parts = UIMessagePart<UIDataTypes, UITools>[];

/** `AssistantWorkspaceProps.status` values this hook produces. */
export type AssistantStatus = 'ready' | 'streaming' | 'error';

// The model/effort picker is hidden (`modelSelector: false` in
// `assistant-config.ts`), so these are inert placeholders required only to
// satisfy `AssistantWorkspaceProps` — never sent to the apiserver.
const INERT_MODEL_ID = '';
const INERT_EFFORT_ID: EffortId = 'high';

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

function textOf(msg: UIMessage): string {
  return msg.parts.find((p): p is TextUIPart => p.type === 'text')?.text ?? '';
}

/**
 * Drives the shared `@datum-cloud/datum-ui/assistant` `AssistantWorkspace`
 * from this plugin's own transport (`src/lib/api.ts`'s `sendMessage` async
 * generator over the apiserver's `sendmessage` subresource — see
 * `src/lib/sse.ts` for the wire event vocabulary), rather than the Vercel AI
 * SDK's `useChat`/`DefaultChatTransport` that cloud-portal's old
 * `use-chat-logic.ts` used. Structurally this is the same hook — chat
 * history (localStorage, scoped per project), a Tiptap editor, speech input
 * — just re-plumbed onto our own SSE envelope instead of the AI SDK's
 * data-stream protocol.
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
  const bottomRef = useRef<HTMLDivElement>(null);

  // ── Chat history (localStorage, scoped per project) ─────────────────────
  const [currentChatId, setCurrentChatId] = useState<string>(() => newId());
  const currentChatIdRef = useRef(currentChatId);
  currentChatIdRef.current = currentChatId;

  const chatCreatedAtRef = useRef(Date.now());
  const [chatList, setChatList] = useState<StoredChat[]>([]);

  const projectNameRef = useRef(ctx.projectName);
  projectNameRef.current = ctx.projectName;

  useEffect(() => {
    setChatList(ctx.projectName ? listChats(ctx.projectName) : []);
  }, [ctx.projectName]);

  const refreshChatList = useCallback(() => {
    if (projectNameRef.current) setChatList(listChats(projectNameRef.current));
  }, []);

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

  const persist = useCallback((finalMessages: UIMessage[]) => {
    const projectName = projectNameRef.current;
    if (!projectName) return;
    const toSave = finalMessages.filter((m) => m.role !== 'system');
    if (toSave.length === 0) return;
    saveChat(projectName, {
      id: currentChatIdRef.current,
      title: deriveTitle(toSave),
      messages: toSave,
      userHtml: [...htmlByUserMsgIndex.current],
      createdAt: chatCreatedAtRef.current,
      updatedAt: Date.now(),
    });
    refreshChatList();
  }, [refreshChatList]);

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
      if (statusRef.current === 'streaming') return;
      const trimmed = text.trim();
      if (!trimmed) return;

      setError(undefined);
      htmlByUserMsgIndex.current.push(opts?.html ?? `<p>${escapeHtml(trimmed)}</p>`);

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

      // Mirrors the assistant message's `parts` locally so persistence at
      // the end of the turn doesn't have to read it back out of React state.
      let currentParts: Parts = [];
      let lastToolCallId: string | undefined;

      const applyParts = (mutate: (parts: Parts) => Parts) => {
        currentParts = mutate(currentParts);
        const parts = currentParts;
        setMessages((prev) => prev.map((m) => (m.id === assistantId ? { ...m, parts } : m)));
      };

      try {
        for await (const event of sendMessage(
          ctx.pluginFetch,
          ctx.projectName,
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
        if (controller.signal.aborted) {
          setStatus('ready');
        } else {
          setError(err instanceof Error ? err : new Error('The request failed.'));
          setStatus('error');
        }
      } finally {
        abortRef.current = null;
        persist([...priorMessages, userMsg, { id: assistantId, role: 'assistant', parts: currentParts }]);
      }
    },
    [ctx, persist]
  );

  const stop = useCallback(() => {
    abortRef.current?.abort();
  }, []);

  // ── Chat switching ────────────────────────────────────────────────────────
  const startNewChat = useCallback(() => {
    setCurrentChatId(newId());
    chatCreatedAtRef.current = Date.now();
    htmlByUserMsgIndex.current = [];
    setMessages([]);
    setError(undefined);
    setStatus('ready');
  }, []);

  const loadChat = useCallback((chat: StoredChat) => {
    setCurrentChatId(chat.id);
    chatCreatedAtRef.current = chat.createdAt;
    htmlByUserMsgIndex.current = chat.userHtml
      ? chat.userHtml.map(sanitizeUserHtml)
      : chat.messages.filter((m) => m.role === 'user').map((m) => sanitizeUserHtml(textOf(m)));
    setMessages(chat.messages);
    setError(undefined);
    setStatus('ready');
    setTimeout(() => bottomRef.current?.scrollIntoView({ behavior: 'instant' }), 50);
  }, []);

  const onLoadChat = useCallback(
    (chat: ChatSummary) => {
      const full = chatList.find((c) => c.id === chat.id);
      if (full) loadChat(full);
    },
    [chatList, loadChat]
  );

  const onDeleteChat = useCallback(
    (e: ReactMouseEvent, chatId: string) => {
      e.stopPropagation();
      const projectName = projectNameRef.current;
      if (!projectName) return;
      deleteChat(projectName, chatId);
      setChatList(listChats(projectName));
      if (chatId === currentChatIdRef.current) startNewChat();
    },
    [startNewChat]
  );

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
            const html = editor?.getHTML() ?? `<p>${escapeHtml(text)}</p>`;
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
      void runTurn(suggestion, { html: `<p>${escapeHtml(suggestion)}</p>` });
    },
    [runTurn]
  );

  const onRetry = useCallback(() => {
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
  }, [runTurn]);

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
    const currentChat = chatList.find((c) => c.id === currentChatId);
    if (currentChat) return currentChat.title;
    return messages.length > 0 ? deriveTitle(messages) : 'New chat';
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
