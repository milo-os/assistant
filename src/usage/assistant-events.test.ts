// LIFTED VERBATIM from cloud-portal
//   branch:  feat/patch-dynamic-composition
//   path:    app/modules/usage/assistant-events.test.ts
// -----------------------------------------------------------------------
import { buildAssistantToolInvocationEvent, buildAssistantUsageEvents } from './assistant-events';
import { ASSISTANT_METERS, ASSISTANT_RESOURCE_GROUP, ASSISTANT_RESOURCE_KIND } from './meters';
import { toCloudEvent } from './to-cloud-event';
import { isUlid } from './ulid';
import { describe, expect, it } from 'bun:test';

const NOW = Date.parse('2026-07-11T12:00:00.000Z');

describe('buildAssistantToolInvocationEvent', () => {
  const input = {
    projectName: 'demo-project',
    conversationId: 'conv-123',
    serviceName: 'streaming.streamco.example',
    now: NOW,
  };

  it('builds one event on the tool-invocations meter with a service dimension', () => {
    const event = buildAssistantToolInvocationEvent(input);

    expect(event.meterName).toBe('assistant.miloapis.com/conversation/tool-invocations');
    expect(event.meterName).toBe(ASSISTANT_METERS.toolInvocations);
    expect(event.value).toBe('1');
    expect(event.dimensions).toEqual({ service: 'streaming.streamco.example' });
    expect(event.timestamp).toBe('2026-07-11T12:00:00.000Z');
    expect(event.projectRef).toEqual({ name: 'demo-project' });
    expect(isUlid(event.eventID)).toBe(true);
  });

  it('attributes the event to the Conversation resource, mirroring the token meters', () => {
    const event = buildAssistantToolInvocationEvent({ ...input, conversationUid: 'uid-1' });

    expect(event.resource.ref).toEqual({
      projectRef: { name: 'demo-project' },
      group: ASSISTANT_RESOURCE_GROUP,
      kind: ASSISTANT_RESOURCE_KIND,
      namespace: 'default',
      name: 'conv-123',
      uid: 'uid-1',
    });
    expect(event.resource.labels).toEqual({ service: 'streaming.streamco.example' });
  });

  it('generates a fresh eventID per invocation (dedup key is per logical sample)', () => {
    const a = buildAssistantToolInvocationEvent(input);
    const b = buildAssistantToolInvocationEvent(input);
    expect(a.eventID).not.toBe(b.eventID);
  });

  it('satisfies the CloudEvents envelope rules end to end', () => {
    const event = buildAssistantToolInvocationEvent(input);
    const cloudEvent = toCloudEvent(event, { source: 'http://portal.test/api/assistant' });

    expect(cloudEvent.type).toBe('assistant.miloapis.com/conversation/tool-invocations');
    expect(cloudEvent.subject).toBe('projects/demo-project');
    expect(cloudEvent.datacontenttype).toBe('application/json');
    expect(cloudEvent.data.value).toBe('1');
    expect(cloudEvent.data.dimensions).toEqual({ service: 'streaming.streamco.example' });
  });
});

describe('buildAssistantUsageEvents (regression: token meters unchanged)', () => {
  it('emits one event per non-zero token axis plus the messages meter, dimensioned by model', () => {
    const events = buildAssistantUsageEvents({
      projectName: 'demo-project',
      conversationId: 'conv-123',
      model: 'claude-sonnet-4-6',
      tokens: { inputTokens: 10, outputTokens: 20, cachedInputTokens: 0 },
      now: NOW,
    });

    expect(events.map((e) => e.meterName)).toEqual([
      ASSISTANT_METERS.inputTokens,
      ASSISTANT_METERS.outputTokens,
      ASSISTANT_METERS.messages,
    ]);
    for (const event of events) {
      expect(event.dimensions).toEqual({ model: 'claude-sonnet-4-6' });
      expect(isUlid(event.eventID)).toBe(true);
    }
  });
});
