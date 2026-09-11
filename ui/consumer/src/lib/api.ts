import type { PluginFetch } from '@datum-cloud/portal-plugin-sdk';
import { parseAssistantEventStream, type AssistantStreamEvent } from './sse';

/**
 * Fetch wrapper for the assistant's aggregated apiserver resources, group
 * `assistant.miloapis.com/v1alpha1` (assistant repo Part 1 —
 * `internal/apiserver/registry/conversation/`). Every call here goes through
 * the caller-supplied `pluginFetch` (`@datum-cloud/portal-plugin-sdk`'s
 * `usePluginFetch()`), which is already scoped to the current project's
 * control plane and authenticated through the portal's Milo proxy — this
 * file never picks a base URL or auth itself.
 *
 * The K8s *namespace* segment, however, must still be the real project name,
 * NOT a fixed "default": `usePluginFetch()`'s control-plane proxy stamps the
 * caller's identity with the real project as `iam.miloapis.com/parent-name`
 * on every request (milo's `ProjectContextAuthorizationDecorator`), and this
 * service's `internal/tenant.ProjectFromContext` requires that stamped
 * project to equal the namespace exactly, 403ing otherwise (a project-scoped
 * token must not be able to read another project's rows by aiming the
 * namespace elsewhere). So every call here takes the project name and uses
 * it as the namespace — the one piece `usePluginFetch()` scopes the URL
 * *prefix* by but doesn't put in the K8s path itself.
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
  status?: {
    lastActiveAt?: string;
    messageCount?: number;
    title?: string;
    name?: string;
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

/** `GET .../conversations` — this project's conversation list. */
export async function listConversations(
  pluginFetch: PluginFetch,
  projectName: string
): Promise<Conversation[]> {
  const response = await pluginFetch(conversationsPath(projectName), {
    headers: { Accept: 'application/json' },
  });
  await assertOk(response);
  const data = (await response.json()) as ConversationList;
  return data.items ?? [];
}

/** `GET .../conversations/{name}/messages` — a conversation's transcript. */
export async function getConversationMessages(
  pluginFetch: PluginFetch,
  projectName: string,
  conversationName: string
): Promise<StoredMessage[]> {
  const response = await pluginFetch(
    `${conversationsPath(projectName)}/${encodeURIComponent(conversationName)}/messages`,
    { headers: { Accept: 'application/json' } }
  );
  await assertOk(response);
  const data = (await response.json()) as MessageList;
  return data.items ?? [];
}

/**
 * `POST .../conversations/{name}/sendmessage` — the streaming subresource.
 * Returns an async generator of `AssistantStreamEvent`s as they arrive over
 * SSE; the caller drives it (e.g. in a `for await` loop) to update UI state
 * incrementally. Aborting `signal` cancels the underlying fetch/stream.
 */
export async function* sendMessage(
  pluginFetch: PluginFetch,
  projectName: string,
  conversationName: string,
  body: SendMessageRequest,
  signal?: AbortSignal
): AsyncGenerator<AssistantStreamEvent> {
  const response = await pluginFetch(
    `${conversationsPath(projectName)}/${encodeURIComponent(conversationName)}/sendmessage`,
    {
      method: 'POST',
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
