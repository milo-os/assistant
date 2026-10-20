import type { PluginProjectContextValue } from '@datum-cloud/portal-plugin-sdk';
import type { PortalPluginHostBindings } from '@datum-cloud/portal-plugin-sdk/host';
import { PortalPluginHostProvider } from '@datum-cloud/portal-plugin-sdk/host';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import type { UIMessage } from 'ai';
import { StrictMode, type ComponentProps, type ReactNode } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import ChatDock from './chat-dock';

const messageList = (messages: UIMessage[]) => (
  <ul data-testid="messages">
    {messages.map((m) => (
      <li key={m.id}>
        {m.role}:{m.parts.map((p) => (p.type === 'text' ? p.text : '')).join('')}
      </li>
    ))}
  </ul>
);

// The real `useAssistantWorkspace` runs underneath; only the presentational
// pieces are stubbed so the test can read the hook's state back out of them.
// The dock renders `AssistantWorkspace`; inline composes `Conversation` and
// `PromptCard` itself.
vi.mock('@datum-cloud/datum-ui/assistant', () => ({
  defaultRenderLink: () => null,
  AssistantConfigProvider: ({ children }: { children: ReactNode }) => (
    <div data-testid="config-provider">{children}</div>
  ),
  Conversation: (props: { messages: UIMessage[]; footer: ReactNode; density?: string }) => (
    <div data-testid="conversation" data-density={props.density ?? 'default'}>
      {messageList(props.messages)}
      {props.footer}
    </div>
  ),
  PromptCard: (props: { isReady: boolean }) => (
    <div data-testid="composer" data-ready={String(props.isReady)} />
  ),
  AssistantWorkspace: (props: {
    orientation?: string;
    onClose?: () => void;
    currentChatId: string;
    messages: UIMessage[];
    density?: string;
  }) => (
    <div
      data-testid="workspace"
      data-orientation={props.orientation}
      data-density={props.density ?? 'default'}
    >
      {props.onClose && (
        <button type="button" onClick={props.onClose}>
          Close
        </button>
      )}
      <span data-testid="chat-id">{props.currentChatId}</span>
      {messageList(props.messages)}
    </div>
  ),
}));

/** One `sendmessage` request, with a hand-driven SSE stream. */
interface Turn {
  chatId: string;
  projectId: string | null;
  text: string;
  /** The parsed request body, to check exactly which keys were sent. */
  body: Record<string, unknown>;
  finish: (reply?: string) => void;
}

let turns: Turn[];
/** Conversation ids whose transcript was fetched, in order. */
let loads: string[];
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
  const requestBody = JSON.parse(String(init?.body)) as Record<string, unknown> & { text: string };
  turns.push({
    chatId: decodeURIComponent(match[1]!),
    projectId: url.searchParams.get('projectId'),
    text: requestBody.text,
    body: requestBody,
    finish: (reply = 'ok') => {
      controller.enqueue(sse({ type: 'text_delta', text: reply }));
      controller.enqueue(sse({ type: 'done', state: 'completed' }));
      controller.close();
    },
  });
  return Promise.resolve(new Response(body, { status: 200 }));
}

// A stored transcript is one user message naming its chat, so a test can tell
// which conversation the dock is showing.
const pluginFetch = vi.fn(async (input: RequestInfo | URL) => {
  const match = String(input).match(/\/conversations\/([^/?]+)\/messages/);
  if (!match) return Response.json({ items: [] });
  const id = decodeURIComponent(match[1]!);
  loads.push(id);
  return Response.json({ items: [{ seq: 1, role: 'user', content: `hi from ${id}` }] });
});

const bindings: PortalPluginHostBindings = {
  useProjectContext: () => ({ project, org: undefined, isLoading: false, error: null }),
  usePluginFetch: () => pluginFetch,
  useResourceWatch: () => ({ lastEvent: null, isConnected: false, error: null }),
};

type DockProps = ComponentProps<typeof ChatDock>;

function renderDock(prompt?: { id: string; text: string }) {
  const { rerender } = renderDockWith({ prompt });
  return { rerender: (next?: { id: string; text: string }) => rerender({ prompt: next }) };
}

function renderDockWith(props: DockProps) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrap = (node: ReactNode) => (
    <StrictMode>
      <QueryClientProvider client={queryClient}>
        <PortalPluginHostProvider bindings={bindings}>{node}</PortalPluginHostProvider>
      </QueryClientProvider>
    </StrictMode>
  );
  const utils = render(wrap(<ChatDock {...props} />));
  return { rerender: (next: DockProps) => utils.rerender(wrap(<ChatDock {...next} />)) };
}

const chatId = () => screen.getByTestId('chat-id').textContent;
const shownMessages = () =>
  Array.from(screen.getByTestId('messages').querySelectorAll('li'), (li) => li.textContent);

/** Lets effects, the stream reader and any follow-up commits settle. */
const settle = () => act(() => new Promise((resolve) => setTimeout(resolve, 20)));

beforeEach(() => {
  turns = [];
  loads = [];
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

// Under StrictMode the hook's project effect starts a new chat on its effect
// re-run, replacing a load issued on mount, so the dock loads the handed chat
// a second time. Each test takes the loads after mount as its baseline.
const mountLoads = (id: string) => {
  expect(loads.length).toBeGreaterThan(0);
  expect(loads.every((l) => l === id)).toBe(true);
  return loads.length;
};

describe('ChatDock chatId', () => {
  it('opens the chat it is handed', async () => {
    renderDockWith({ chatId: 'chat-a' });

    await waitFor(() => expect(chatId()).toBe('chat-a'));
    await waitFor(() => expect(shownMessages()).toEqual(['user:hi from chat-a']));
    await settle();
    expect(chatId()).toBe('chat-a');
    mountLoads('chat-a');
  });

  it('does not reload the same chatId', async () => {
    const { rerender } = renderDockWith({ chatId: 'chat-a' });
    await waitFor(() => expect(shownMessages()).toEqual(['user:hi from chat-a']));
    await settle();
    const baseline = mountLoads('chat-a');

    rerender({ chatId: 'chat-a' });
    await settle();

    expect(loads).toHaveLength(baseline);
    expect(chatId()).toBe('chat-a');
  });

  it('switches to a new chatId', async () => {
    const { rerender } = renderDockWith({ chatId: 'chat-a' });
    await waitFor(() => expect(shownMessages()).toEqual(['user:hi from chat-a']));
    await settle();
    const baseline = mountLoads('chat-a');

    rerender({ chatId: 'chat-b' });

    await waitFor(() => expect(shownMessages()).toEqual(['user:hi from chat-b']));
    await settle();
    expect(chatId()).toBe('chat-b');
    expect(loads.slice(baseline)).toEqual(['chat-b']);
  });

  it('lets a new prompt win over a chatId passed with it', async () => {
    const props: DockProps = { prompt: { id: 'p1', text: 'hello' }, chatId: 'chat-a' };
    const { rerender } = renderDockWith(props);

    await waitFor(() => expect(turns).toHaveLength(1));
    await act(async () => turns[0]!.finish());
    rerender(props);
    await settle();

    expect(loads).toEqual([]);
    expect(turns).toHaveLength(1);
    expect(turns[0]!.chatId).not.toBe('chat-a');
    expect(chatId()).toBe(turns[0]!.chatId);
    expect(shownMessages()).toEqual(['user:hello', 'assistant:ok']);
  });
});

describe('ChatDock onChatChange', () => {
  it('reports the chat on mount and on change, not on re-render', async () => {
    const onChatChange = vi.fn();
    const reported = () => onChatChange.mock.calls.map(([id]) => id as string);
    const { rerender } = renderDockWith({ onChatChange });
    await settle();
    // StrictMode's extra new chat on mount (see above) is a real change, so
    // it may be reported too; the last report is the chat on screen and no
    // id is reported twice in a row.
    expect(reported().at(-1)).toBe(chatId());
    expect(reported().every((id, i, all) => id !== all[i - 1])).toBe(true);
    const mounted = reported().length;

    // A re-render, even with a fresh callback identity, is not a change.
    rerender({ onChatChange: (id: string) => onChatChange(id) });
    await settle();
    expect(reported()).toHaveLength(mounted);

    rerender({ onChatChange, chatId: 'chat-a' });
    await waitFor(() => expect(chatId()).toBe('chat-a'));
    await settle();
    expect(reported().slice(mounted)).toEqual(['chat-a']);
  });
});

describe('ChatDock placement', () => {
  it('renders the vertical workspace with its close control in the dock', async () => {
    const onClose = vi.fn();
    renderDockWith({ onClose });
    expect(screen.getByTestId('workspace').dataset.orientation).toBe('vertical');
    // The dock keeps the workspace's own (default) density.
    expect(screen.getByTestId('workspace').dataset.density).toBe('default');
    expect(screen.queryByTestId('conversation')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    expect(onClose).toHaveBeenCalledTimes(1);
    await settle();
  });

  it('renders only a placeholder and the composer inline, without the workspace shell', async () => {
    renderDockWith({ onClose: vi.fn(), placement: 'inline' });
    await settle();

    expect(screen.queryByTestId('workspace')).toBeNull();
    expect(screen.queryByTestId('assistant-dock')).toBeNull();
    expect(screen.queryByRole('button')).toBeNull();
    expect(screen.queryByTestId('conversation')).toBeNull();
    expect(screen.getByText('Ask Patch anything about this project.')).toBeTruthy();
    // The composer sits inside the config provider, so it gets Patch's config.
    const composer = screen.getByTestId('composer');
    expect(screen.getByTestId('config-provider').contains(composer)).toBe(true);
    expect(composer.dataset.ready).toBe('true');
  });

  it('sends a prompt inline into the composed conversation', async () => {
    renderDockWith({ placement: 'inline', prompt: { id: 'p1', text: 'hello' } });

    await waitFor(() => expect(turns).toHaveLength(1));
    await act(async () => turns[0]!.finish('hi'));
    await waitFor(() => expect(shownMessages()).toEqual(['user:hello', 'assistant:hi']));
    expect(turns[0]).toMatchObject({ projectId: 'demo-project', text: 'hello' });
    expect(screen.queryByTestId('workspace')).toBeNull();
    // The composer is pinned as the conversation's footer.
    expect(screen.getByTestId('conversation').contains(screen.getByTestId('composer'))).toBe(true);
    expect(screen.queryByText('Ask Patch anything about this project.')).toBeNull();
  });

  it('renders the inline conversation at compact density', async () => {
    renderDockWith({ placement: 'inline', prompt: { id: 'p1', text: 'hello' } });

    await waitFor(() => expect(turns).toHaveLength(1));
    await act(async () => turns[0]!.finish('hi'));
    await waitFor(() => expect(shownMessages()).toEqual(['user:hello', 'assistant:hi']));
    expect(screen.getByTestId('conversation').dataset.density).toBe('compact');
  });

  it('loads a handed chatId inline and reports it', async () => {
    const onChatChange = vi.fn();
    renderDockWith({ placement: 'inline', chatId: 'chat-a', onChatChange });

    await waitFor(() => expect(shownMessages()).toEqual(['user:hi from chat-a']));
    await settle();
    mountLoads('chat-a');
    expect(onChatChange.mock.calls.at(-1)?.[0]).toBe('chat-a');
    expect(screen.queryByTestId('workspace')).toBeNull();
  });
});

describe('ChatDock surface', () => {
  const surface = { name: 'home-first-run', network: 'default' } as const;

  it('sends the surface with each turn', async () => {
    renderDockWith({ placement: 'inline', surface, prompt: { id: 'p1', text: 'hello' } });

    await waitFor(() => expect(turns).toHaveLength(1));
    expect(turns[0]!.body).toEqual({ text: 'hello', surface });
  });

  it('sends no surface key when none is set', async () => {
    renderDockWith({ prompt: { id: 'p1', text: 'hello' } });

    await waitFor(() => expect(turns).toHaveLength(1));
    expect(turns[0]!.body).toEqual({ text: 'hello' });
    expect('surface' in turns[0]!.body).toBe(false);
  });

  it('sends a changed surface on the next turn without starting a new chat', async () => {
    const { rerender } = renderDockWith({
      placement: 'inline',
      surface: { name: 'home-first-run' },
      prompt: { id: 'p1', text: 'first' },
    });
    await waitFor(() => expect(turns).toHaveLength(1));
    await act(async () => turns[0]!.finish());
    await waitFor(() => expect(shownMessages()).toEqual(['user:first', 'assistant:ok']));

    // Same prompt id, so the only change is the surface: no new chat.
    rerender({ placement: 'inline', surface, prompt: { id: 'p1', text: 'first' } });
    await settle();
    expect(turns).toHaveLength(1);
    expect(shownMessages()).toEqual(['user:first', 'assistant:ok']);

    // The next turn sent (here the host's next prompt) carries the new surface.
    rerender({ placement: 'inline', surface, prompt: { id: 'p2', text: 'second' } });
    await waitFor(() => expect(turns).toHaveLength(2));
    expect(turns[0]!.body).toEqual({ text: 'first', surface: { name: 'home-first-run' } });
    expect(turns[1]!.body).toEqual({ text: 'second', surface });
  });
});
