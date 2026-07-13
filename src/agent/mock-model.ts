// src/agent/mock-model.ts
//
// MODEL_MODE=mock language model. Built on ai@6's MockLanguageModelV3
// test util (from `ai/test`). It exists so the FULL chat path — tool
// call over real MCP, tool result folded into a final answer, usage
// reported — is provable without an ANTHROPIC_API_KEY or any network to
// a model provider.
//
// Script (per the build contract):
//   1. If the latest user message mentions "diagnose" AND a tool whose
//      name matches /pipeline_diagnose/ is available, emit a single tool
//      call to it (finishReason: tool-calls). streamText executes the
//      tool (real MCP round-trip when composed against a live server).
//   2. On the follow-up step the prompt now carries the tool result, so
//      the mock emits final text that quotes the tool's findings
//      (finishReason: stop).
//   3. Otherwise emit a short generic reply.
// Every response reports fake-but-nonzero token usage.
//
// PARITY CAVEAT (documented in README): this is a canned script, not a
// language model. It proves plumbing and event shapes, NOT answer
// quality or real tool-selection behaviour — only MODEL_MODE=anthropic
// exercises those.
import { MockLanguageModelV3 } from 'ai/test';
import type {
  LanguageModelV3,
  LanguageModelV3CallOptions,
  LanguageModelV3FinishReason,
  LanguageModelV3FunctionTool,
  LanguageModelV3Prompt,
  LanguageModelV3StreamPart,
  LanguageModelV3ToolResultOutput,
  LanguageModelV3Usage,
} from '@ai-sdk/provider';

/** The unified finish reason strings (the object wrapper is built below). */
type FinishReasonKind = LanguageModelV3FinishReason['unified'];

/** Fake-but-nonzero usage every mock response reports. */
export const MOCK_USAGE: LanguageModelV3Usage = {
  inputTokens: { total: 42, noCache: 42, cacheRead: 0, cacheWrite: 0 },
  outputTokens: { total: 23, text: 23, reasoning: 0 },
};

export const MOCK_MODEL_ID = 'patch-mock-v0';

export function createMockLanguageModel(): LanguageModelV3 {
  return new MockLanguageModelV3({
    modelId: MOCK_MODEL_ID,
    provider: 'patch-mock',
    doStream: scriptedDoStream,
  });
}

const scriptedDoStream: LanguageModelV3['doStream'] = async (
  options: LanguageModelV3CallOptions
) => {
  const toolResult = extractLatestToolResult(options.prompt);
  const userText = latestUserText(options.prompt);

  let parts: LanguageModelV3StreamPart[];
  if (toolResult !== undefined) {
    parts = textParts(summarizeToolResult(toolResult), 'stop');
  } else {
    const diagnoseTool = wantsDiagnose(userText) ? findDiagnoseTool(options.tools) : undefined;
    if (diagnoseTool) {
      parts = toolCallParts(diagnoseTool, { id: extractPipelineId(userText) });
    } else {
      parts = textParts(genericReply(userText), 'stop');
    }
  }

  return { stream: streamFromParts(parts) };
};

// ── Prompt inspection ─────────────────────────────────────────

function latestUserText(prompt: LanguageModelV3Prompt): string {
  for (let i = prompt.length - 1; i >= 0; i--) {
    const message = prompt[i]!;
    if (message.role !== 'user') continue;
    return message.content
      .filter((part): part is { type: 'text'; text: string } => part.type === 'text')
      .map((part) => part.text)
      .join(' ')
      .trim();
  }
  return '';
}

/**
 * Return the text of the most recent tool result in the prompt, or
 * undefined if the model has not yet seen a tool result. Handles every
 * tool-result output variant, falling back to a JSON dump so provider
 * findings always survive into the summary.
 */
function extractLatestToolResult(prompt: LanguageModelV3Prompt): string | undefined {
  for (let i = prompt.length - 1; i >= 0; i--) {
    const message = prompt[i]!;
    if (message.role !== 'tool' && message.role !== 'assistant') continue;
    for (let j = message.content.length - 1; j >= 0; j--) {
      const part = message.content[j]!;
      if (part.type === 'tool-result') {
        return stringifyToolOutput(part.output);
      }
    }
  }
  return undefined;
}

function stringifyToolOutput(output: LanguageModelV3ToolResultOutput): string {
  switch (output.type) {
    case 'text':
    case 'error-text':
      return output.value;
    case 'json':
    case 'error-json':
      return safeJson(output.value);
    case 'content':
      return output.value
        .filter((c): c is { type: 'text'; text: string } => c.type === 'text')
        .map((c) => c.text)
        .join(' ')
        .trim();
    case 'execution-denied':
      return `tool execution denied${output.reason ? `: ${output.reason}` : ''}`;
    default:
      return safeJson(output);
  }
}

function wantsDiagnose(userText: string): boolean {
  return /diagnose/i.test(userText);
}

function findDiagnoseTool(
  tools: LanguageModelV3CallOptions['tools']
): string | undefined {
  const functionTools = (tools ?? []).filter(
    (t): t is LanguageModelV3FunctionTool => t.type === 'function'
  );
  return functionTools.find((t) => /pipeline_diagnose/i.test(t.name))?.name;
}

function extractPipelineId(userText: string): string {
  const explicit = /\bp-[a-z0-9]+\b/i.exec(userText);
  if (explicit) return explicit[0];
  const afterPipeline = /pipeline\s+([^\s.,;]+)/i.exec(userText);
  if (afterPipeline?.[1]) return afterPipeline[1];
  return 'p-1';
}

// ── Response templates ────────────────────────────────────────

function summarizeToolResult(toolResultText: string): string {
  const compact = toolResultText.replace(/\s+/g, ' ').trim().slice(0, 800);
  return `Ran the pipeline diagnosis. The provider tool reported: ${compact}. In short, that's the signal to chase down.`;
}

function genericReply(userText: string): string {
  if (!userText) {
    return "I'm Patch, the Datum Cloud assistant. Ask me about this project, its resources, or a provider service entitled to it.";
  }
  return `I'm Patch (running in mock mode, so this is a canned reply). You said: "${truncate(userText, 200)}". Ask me to diagnose a provider pipeline to see the tool path in action.`;
}

// ── Stream part builders ──────────────────────────────────────

function textParts(text: string, finishReason: FinishReasonKind): LanguageModelV3StreamPart[] {
  const id = 'mock-text-0';
  const chunks = chunkText(text);
  return [
    { type: 'stream-start', warnings: [] },
    { type: 'text-start', id },
    ...chunks.map((delta): LanguageModelV3StreamPart => ({ type: 'text-delta', id, delta })),
    { type: 'text-end', id },
    { type: 'finish', finishReason: { unified: finishReason, raw: undefined }, usage: MOCK_USAGE },
  ];
}

function toolCallParts(
  toolName: string,
  args: Record<string, unknown>
): LanguageModelV3StreamPart[] {
  const toolCallId = 'mock-tool-call-0';
  const input = JSON.stringify(args);
  return [
    { type: 'stream-start', warnings: [] },
    { type: 'tool-input-start', id: toolCallId, toolName },
    { type: 'tool-input-delta', id: toolCallId, delta: input },
    { type: 'tool-input-end', id: toolCallId },
    { type: 'tool-call', toolCallId, toolName, input },
    { type: 'finish', finishReason: { unified: 'tool-calls', raw: undefined }, usage: MOCK_USAGE },
  ];
}

function streamFromParts(
  parts: LanguageModelV3StreamPart[]
): ReadableStream<LanguageModelV3StreamPart> {
  return new ReadableStream<LanguageModelV3StreamPart>({
    start(controller) {
      for (const part of parts) controller.enqueue(part);
      controller.close();
    },
  });
}

/** Split into ~6-word chunks so the SSE stream shows incremental text. */
function chunkText(text: string): string[] {
  const words = text.split(/(\s+)/).filter((w) => w.length > 0);
  const chunks: string[] = [];
  let buffer = '';
  let wordCount = 0;
  for (const token of words) {
    buffer += token;
    if (/\S/.test(token)) wordCount++;
    if (wordCount >= 6) {
      chunks.push(buffer);
      buffer = '';
      wordCount = 0;
    }
  }
  if (buffer) chunks.push(buffer);
  return chunks.length > 0 ? chunks : [text];
}

function truncate(text: string, max: number): string {
  return text.length > max ? `${text.slice(0, max)}…` : text;
}

function safeJson(value: unknown): string {
  try {
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
}
