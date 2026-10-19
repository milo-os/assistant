import type { PluginProjectContextValue } from '@datum-cloud/portal-plugin-sdk';
import type { PortalPluginHostBindings } from '@datum-cloud/portal-plugin-sdk/host';
import { PortalPluginHostProvider } from '@datum-cloud/portal-plugin-sdk/host';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, cleanup, render, screen, waitFor } from '@testing-library/react';
import type { UIMessage } from 'ai';
import { StrictMode, type ReactNode } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import ChatDock from './chat-dock';

// The real `useAssistantWorkspace` runs underneath; only the presentational
// shell is stubbed so the test can read the hook's state back out of it.
vi.mock('@datum-cloud/datum-ui/assistant', () => ({
  defaultRenderLink: () => null,
  AssistantWorkspace: (props: { currentChatId: string; messages: UIMessage[] }) => (
    <div>
      <span data-testid="chat-id">{props.currentChatId}</span>
      <ul data-testid="messages">
        {props.messages.map((m) => (
          <li key={m.id}>
            {m.role}:{m.parts.map((p) => (p.type === 'text' ? p.text : '')).join('')}
          </li>
        ))}
      </ul>
    </div>
  ),
}));

/** One `sendmessage` request, with a hand-driven SSE stream. */
interface Turn {
  chatId: string;
  projectId: string | null;
  text: string;
  finish: (reply?: string) => void;
}

let turns: Turn[];
let project: PluginProjectContextValue['project'];

const encoder = new TextEncoder();
const sse = (event: object) => encoder.encode(`data: ${JSON.stringify(event)}\n\n`);

function fakeFetch(input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
  const url = new URL(String(input), 'http://portal.test');
  const match = url.pathname.match(/\/conversations\/([^/]+)\/sendmessage$/);
  if (!match) return Promise.reject(new Error(`unexpected fetch ${url.pathname}`));

  let controller!: ReadableStreamDefaultController<Uint8Array>;
  const body = new ReadableStream<Uint8Array>({
    start(c) {
      controller = c;
    },
  });
  init?.signal?.addEventListener('abort', () => controller.error(new DOMException('aborted', 'AbortError')));
  turns.push({
    chatId: decodeURIComponent(match[1]!),
    projectId: url.searchParams.get('projectId'),
    text: (JSON.parse(String(init?.body)) as { text: string }).text,
    finish: (reply = 'ok') => {
      controller.enqueue(sse({ type: 'text_delta', text: reply }));
      controller.enqueue(sse({ type: 'done', state: 'completed' }));
      controller.close();
    },
  });
  return Promise.resolve(new Response(body, { status: 200 }));
}

const pluginFetch = vi.fn(async () => Response.json({ items: [] }));

const bindings: PortalPluginHostBindings = {
  useProjectContext: () => ({ project, org: undefined, isLoading: false, error: null }),
  usePluginFetch: () => pluginFetch,
  useResourceWatch: () => ({ lastEvent: null, isConnected: false, error: null }),
};

function renderDock(prompt?: { id: string; text: string }) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrap = (node: ReactNode) => (
    <StrictMode>
      <QueryClientProvider client={queryClient}>
        <PortalPluginHostProvider bindings={bindings}>{node}</PortalPluginHostProvider>
      </QueryClientProvider>
    </StrictMode>
  );
  const utils = render(wrap(<ChatDock prompt={prompt} />));
  return {
    rerender: (next?: { id: string; text: string }) => utils.rerender(wrap(<ChatDock prompt={next} />)),
  };
}

const chatId = () => screen.getByTestId('chat-id').textContent;
const shownMessages = () =>
  Array.from(screen.getByTestId('messages').querySelectorAll('li'), (li) => li.textContent);

/** Lets effects, the stream reader and any follow-up commits settle. */
const settle = () => act(() => new Promise((resolve) => setTimeout(resolve, 20)));

beforeEach(() => {
  turns = [];
  project = { name: 'demo-project', displayName: 'Demo Project' };
  vi.stubGlobal('fetch', vi.fn(fakeFetch));
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('ChatDock prompt', () => {
  it('sends a prompt once on mount, into the chat it is showing', async () => {
    renderDock({ id: 'p1', text: 'How do I set up a network?' });

    await waitFor(() => expect(turns).toHaveLength(1));
    await settle();
    expect(turns).toHaveLength(1);
    expect(turns[0]).toMatchObject({
      chatId: chatId(),
      projectId: 'demo-project',
      text: 'How do I set up a network?',
    });
    expect(shownMessages()).toEqual(['user:How do I set up a network?', 'assistant:']);
  });

  it('ignores a re-render with the same prompt id', async () => {
    const { rerender } = renderDock({ id: 'p1', text: 'hello' });
    await waitFor(() => expect(turns).toHaveLength(1));
    await act(async () => turns[0]!.finish());
    const firstChat = chatId();

    rerender({ id: 'p1', text: 'hello' });
    await settle();

    expect(turns).toHaveLength(1);
    expect(chatId()).toBe(firstChat);
  });

  it('starts another new chat for a new prompt id, even with the same text', async () => {
    const { rerender } = renderDock({ id: 'p1', text: 'hello' });
    await waitFor(() => expect(turns).toHaveLength(1));
    await act(async () => turns[0]!.finish('first reply'));
    await waitFor(() => expect(shownMessages()).toEqual(['user:hello', 'assistant:first reply']));
    const firstChat = chatId();

    rerender({ id: 'p2', text: 'hello' });

    await waitFor(() => expect(turns).toHaveLength(2));
    await settle();
    expect(turns).toHaveLength(2);
    expect(turns[1]!.chatId).not.toBe(firstChat);
    expect(turns[1]!.chatId).toBe(chatId());
    // The previous chat's transcript is not carried into the new one.
    expect(shownMessages()).toEqual(['user:hello', 'assistant:']);
  });

  it('waits for an in-flight turn to finish before sending a new prompt', async () => {
    const { rerender } = renderDock({ id: 'p1', text: 'first' });
    await waitFor(() => expect(turns).toHaveLength(1));

    rerender({ id: 'p2', text: 'second' });
    await settle();
    // Not sent, and the running turn was not aborted by a new chat.
    expect(turns).toHaveLength(1);
    expect(chatId()).toBe(turns[0]!.chatId);

    await act(async () => turns[0]!.finish());

    await waitFor(() => expect(turns).toHaveLength(2));
    expect(turns[1]).toMatchObject({ text: 'second', chatId: chatId() });
    expect(turns[1]!.chatId).not.toBe(turns[0]!.chatId);
    expect(shownMessages()).toEqual(['user:second', 'assistant:']);
  });

  it('holds a prompt until the project resolves', async () => {
    project = undefined;
    const { rerender } = renderDock({ id: 'p1', text: 'hello' });
    await settle();
    expect(turns).toHaveLength(0);

    project = { name: 'demo-project' };
    rerender({ id: 'p1', text: 'hello' });

    await waitFor(() => expect(turns).toHaveLength(1));
    await settle();
    expect(turns).toHaveLength(1);
    expect(turns[0]).toMatchObject({ projectId: 'demo-project', chatId: chatId() });
  });

  it('sends nothing and keeps its chat when there is no prompt', async () => {
    const { rerender } = renderDock();
    await settle();
    const initialChat = chatId();

    rerender();
    await settle();

    expect(turns).toHaveLength(0);
    expect(chatId()).toBe(initialChat);
    expect(shownMessages()).toEqual([]);
  });
});
