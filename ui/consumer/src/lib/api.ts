import type { PluginFetch } from '@datum-cloud/portal-plugin-sdk';
import { parseAssistantEventStream, type AssistantStreamEvent } from './sse';

/**
 * Fetch wrapper for the assistant's read views and its chat-send call.
 *
 * `listConversations`/`getConversationMessages` go through the caller-supplied
 * `pluginFetch` (`@datum-cloud/portal-plugin-sdk`'s `usePluginFetch()`)
 * against the aggregated apiserver's resources, group
 * `assistant.miloapis.com/v1alpha1` (assistant repo Part 1 —
 * `internal/apiserver/registry/conversation/`) — already scoped to the
 * current project's control plane and authenticated through the portal's
 * Milo proxy.
 *
 * The K8s *namespace* segment, however, must still be the real project name,
 * NOT a fixed "default": `usePluginFetch()`'s control-plane proxy stamps the
 * caller's identity with the real project as `iam.miloapis.com/parent-name`
 * on every request (milo's `ProjectContextAuthorizationDecorator`), and this
 * service's `internal/tenant.ProjectFromContext` requires that stamped
 * project to equal the namespace exactly, 403ing otherwise (a project-scoped
 * token must not be able to read another project's rows by aiming the
 * namespace elsewhere). So every read here takes the project name and uses
 * it as the namespace — the one piece `usePluginFetch()` scopes the URL
 * *prefix* by but doesn't put in the K8s path itself.
 *
 * `sendMessage` is different: it does NOT use `pluginFetch` at all. See its
 * own doc comment.
 */

const GROUP_VERSION = 'assistant.miloapis.com/v1alpha1';

/**
 * A conversation as listed by the apiserver's read view — the wire shape of
 * pkg/apis/assistant/v1alpha1.Conversation (a real k8s object: `metadata` +
 * `status`, not a flat DTO).
 */
export interface Conversation {
  metadata: {
    /** Resource name — also the conversation/context id used in URLs. */
    name: string;
    namespace?: string;
    creationTimestamp?: string;
  };
  spec?: {
    /**
     * Restorable hide: archived conversations drop out of the default list
     * and come back via `?fieldSelector=spec.archived=true`. Sending a
     * message to an archived conversation unarchives it server-side.
     */
    archived?: boolean;
  };
  status?: {
    lastActiveAt?: string;
    messageCount?: number;
    title?: string;
    /** User-given name; wins over the generated `title` when set. */
    name?: string;
    /** When `spec.archived` last became true. */
    archivedAt?: string;
  };
}

interface ConversationList {
  items: Conversation[];
}

/** A stored turn in a conversation's transcript (v1alpha1.ConversationMessage). */
export interface StoredMessage {
  seq: number;
  role: 'user' | 'assistant' | 'system' | 'summary';
  content: string;
  createdAt: string;
}

/** Wire shape of v1alpha1.ConversationMessages — the whole-transcript subresource. */
interface MessageList {
  items: StoredMessage[];
}

/** A resource the user pointed at with "@kind/name" — mirrors internal/a2a.Mention. */
export interface Mention {
  kind: string;
  name: string;
  apiGroup?: string;
}

/** Body accepted by the `sendmessage` streaming subresource. */
export interface SendMessageRequest {
  text: string;
  mentions?: Mention[];
}

export interface ApiError extends Error {
  status: number;
}

function makeApiError(status: number, message: string): ApiError {
  const err = new Error(message) as ApiError;
  err.status = status;
  return err;
}

function conversationsPath(projectName: string): string {
  return `/apis/${GROUP_VERSION}/namespaces/${encodeURIComponent(projectName)}/conversations`;
}

async function assertOk(response: Response): Promise<Response> {
  if (!response.ok) {
    const body = await response.text().catch(() => '');
    throw makeApiError(
      response.status,
      body || `request failed with status ${response.status}`
    );
  }
  return response;
}

function conversationPath(projectName: string, conversationName: string): string {
  return `${conversationsPath(projectName)}/${encodeURIComponent(conversationName)}`;
}

/**
 * `GET .../conversations` — this project's conversation list. The server
 * returns only non-archived conversations by default; `archived: true` asks
 * for only the archived ones instead (the two sets are disjoint, so a caller
 * wanting everything fetches both).
 */
export async function listConversations(
  pluginFetch: PluginFetch,
  projectName: string,
  opts?: { archived?: boolean }
): Promise<Conversation[]> {
  const query = opts?.archived
    ? `?fieldSelector=${encodeURIComponent('spec.archived=true')}`
    : '';
  const response = await pluginFetch(`${conversationsPath(projectName)}${query}`, {
    headers: { Accept: 'application/json' },
  });
  await assertOk(response);
  const data = (await response.json()) as ConversationList;
  return data.items ?? [];
}

/**
 * `PATCH .../conversations/{name}` (JSON merge patch on `spec.archived`) —
 * archives or restores a conversation. Returns the updated object.
 */
export async function setConversationArchived(
  pluginFetch: PluginFetch,
  projectName: string,
  conversationName: string,
  archived: boolean
): Promise<Conversation> {
  const response = await pluginFetch(conversationPath(projectName, conversationName), {
    method: 'PATCH',
    headers: {
      'Content-Type': 'application/merge-patch+json',
      Accept: 'application/json',
    },
    body: JSON.stringify({ spec: { archived } }),
  });
  await assertOk(response);
  return (await response.json()) as Conversation;
}

/**
 * `DELETE .../conversations/{name}` — a hard, irreversible delete of the
 * conversation and its transcript. A 404 means it is already gone, which is
 * the outcome the caller asked for, so it resolves rather than throwing.
 */
export async function deleteConversation(
  pluginFetch: PluginFetch,
  projectName: string,
  conversationName: string
): Promise<void> {
  const response = await pluginFetch(conversationPath(projectName, conversationName), {
    method: 'DELETE',
    headers: { Accept: 'application/json' },
  });
  if (response.status === 404) return;
  await assertOk(response);
}

/** `GET .../conversations/{name}/messages` — a conversation's transcript. */
export async function getConversationMessages(
  pluginFetch: PluginFetch,
  projectName: string,
  conversationName: string
): Promise<StoredMessage[]> {
  const response = await pluginFetch(
    `${conversationPath(projectName, conversationName)}/messages`,
    { headers: { Accept: 'application/json' } }
  );
  await assertOk(response);
  const data = (await response.json()) as MessageList;
  return data.items ?? [];
}

/**
 * `POST /api/assistant-chat/conversations/{name}/sendmessage` — turn
 * execution, streamed back as SSE. Deliberately NOT a `pluginFetch` call
 * like every other function in this file: it goes straight to the portal's
 * own origin (same-origin `fetch`, cookie session), which proxies to the
 * assistant's standalone A2A server rather than the aggregated apiserver's
 * `conversations/{id}/sendmessage` subresource. See
 * `cloud-portal/app/server/routes/assistant-chat.ts` for why — in short,
 * identity forwarding to capability providers (compute's MCP tool) only
 * works over A2A's own bearer-token auth path, not through Milo's
 * aggregation-layer impersonation. The portal route translates A2A's wire
 * protocol back into this exact same event vocabulary, so
 * `parseAssistantEventStream`/`AssistantStreamEvent` below need no changes.
 *
 * Returns an async generator of `AssistantStreamEvent`s as they arrive;
 * the caller drives it (e.g. in a `for await` loop) to update UI state
 * incrementally. Aborting `signal` cancels the underlying fetch/stream.
 */
export async function* sendMessage(
  projectName: string,
  conversationName: string,
  body: SendMessageRequest,
  signal?: AbortSignal
): AsyncGenerator<AssistantStreamEvent> {
  const response = await fetch(
    `/api/assistant-chat/conversations/${encodeURIComponent(conversationName)}/sendmessage?projectId=${encodeURIComponent(projectName)}`,
    {
      method: 'POST',
      credentials: 'include',
      headers: {
        'Content-Type': 'application/json',
        Accept: 'text/event-stream',
      },
      body: JSON.stringify(body),
      signal,
    }
  );
  await assertOk(response);
  if (!response.body) {
    throw makeApiError(response.status, 'sendmessage response had no body to stream');
  }
  yield* parseAssistantEventStream(response.body);
}
