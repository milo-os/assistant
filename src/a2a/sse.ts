// src/a2a/sse.ts
//
// Server-Sent Events framing for message/stream. Per A2A, each SSE frame
// carries a full JSON-RPC 2.0 response whose `result` is one stream
// event (Task, TaskStatusUpdateEvent, or TaskArtifactUpdateEvent). The
// stream closes after the frame whose status event has final:true.
import { success, type JsonRpcId } from './jsonrpc';
import type { StreamEvent } from './types';

/** Serialize one stream event as an SSE `data:` frame (JSON-RPC wrapped). */
export function sseFrame(id: JsonRpcId, event: StreamEvent): string {
  return `data: ${JSON.stringify(success(id, event))}\n\n`;
}

/**
 * Build a ReadableStream<Uint8Array> of SSE frames from an async iterable
 * of stream events. Encodes each event with `sseFrame`.
 */
export function sseStreamFrom(
  id: JsonRpcId,
  events: AsyncIterable<StreamEvent>,
  onError?: (err: unknown) => StreamEvent | undefined
): ReadableStream<Uint8Array> {
  const encoder = new TextEncoder();
  return new ReadableStream<Uint8Array>({
    async start(controller) {
      try {
        for await (const event of events) {
          controller.enqueue(encoder.encode(sseFrame(id, event)));
        }
      } catch (err) {
        const fallback = onError?.(err);
        if (fallback) {
          controller.enqueue(encoder.encode(sseFrame(id, fallback)));
        }
      } finally {
        controller.close();
      }
    },
  });
}
