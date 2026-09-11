import type { UIMessage } from 'ai';

/**
 * Local chat history, scoped per project — this plugin keys chats by the
 * SDK's `useProjectContext().project.name` so each project keeps its own
 * conversation list (ported from cloud-portal's `project.name`-scoped
 * localStorage, see `/tmp/old-assistant-ref/chat-storage.ts`). This plugin's
 * chat id doubles as the conversation name sent to the apiserver's
 * `sendmessage` subresource.
 */
const KEY_PREFIX = 'datum:assistant-chats:';
const MAX_CHATS = 50;

/**
 * A locally-persisted chat. Superset of datum-ui's `ChatSummary`
 * (`id`, `title`, `updatedAt`, `messages`) — adds the fields needed to
 * restore a conversation exactly (per-message Tiptap HTML, timestamps).
 */
export interface StoredChat {
  id: string;
  title: string;
  messages: UIMessage[];
  /** Tiptap HTML for each user message, indexed by position in the user-message sub-array. */
  userHtml?: string[];
  createdAt: number;
  updatedAt: number;
}

function storageKey(projectName: string): string {
  return `${KEY_PREFIX}${projectName}`;
}

export function listChats(projectName: string): StoredChat[] {
  try {
    const raw = localStorage.getItem(storageKey(projectName));
    return raw ? (JSON.parse(raw) as StoredChat[]) : [];
  } catch {
    return [];
  }
}

export function saveChat(projectName: string, chat: StoredChat): void {
  try {
    const rest = listChats(projectName).filter((c) => c.id !== chat.id);
    rest.unshift(chat); // most recent first
    localStorage.setItem(storageKey(projectName), JSON.stringify(rest.slice(0, MAX_CHATS)));
  } catch {
    // localStorage may be full or unavailable
  }
}

export function deleteChat(projectName: string, chatId: string): void {
  try {
    const chats = listChats(projectName).filter((c) => c.id !== chatId);
    localStorage.setItem(storageKey(projectName), JSON.stringify(chats));
  } catch {
    // localStorage may be full or unavailable
  }
}

export function deriveTitle(messages: UIMessage[]): string {
  const first = messages.find((m) => m.role === 'user');
  if (!first) return 'New chat';
  const text = first.parts.find((p) => p.type === 'text')?.text ?? '';
  return text.length > 42 ? text.slice(0, 42) + '…' : text || 'New chat';
}
