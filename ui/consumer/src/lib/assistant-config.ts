import { defaultRenderLink } from '@datum-cloud/datum-ui/assistant';
import type { AssistantConfig } from '@datum-cloud/datum-ui/assistant';

/**
 * This plugin's concrete `AssistantConfig` ("Patch"), ported from
 * cloud-portal's `/tmp/old-assistant-ref/cloud-config.tsx` +
 * `constants.ts`. Cloud-portal's suggestions/tool labels leaned on
 * cloud-specific tools (DNS zones, billing, domains) that this generic
 * chat-dock plugin doesn't know about — replaced below with copy generic to
 * an assistant that answers questions about the current project.
 */

/** Starter prompts on the empty/new-chat state. */
export const SUGGESTIONS = [
  'Give me a detailed summary of my project.',
  'What can Patch help me with in this project?',
  'Summarize recent activity in this project.',
  'What resources do I have in this project?',
] as const;

/**
 * Tool name → progress label shown while a tool call is running. This
 * plugin's backend tool set isn't fixed/known ahead of time (unlike
 * cloud-portal's Hono route, which called a fixed list of first-party
 * tools), so this starts empty — the workspace falls back to a generic
 * "Using {tool}…" label for anything not listed here. Add entries here as
 * specific backend tools are worth a friendlier label.
 */
export const TOOL_LABELS: Record<string, string> = {};

export const ASSISTANT_CONFIG: AssistantConfig = {
  greeting: (name) => `Hey there${name ? `, ${name}` : ''}`,
  suggestions: [...SUGGESTIONS],
  // No reasoning-part source on this transport (the apiserver's SSE envelope
  // has no `reasoning`/`thinking` event) — hide the toggle rather than fake it.
  showReasoning: false,
  // No model/effort picker — the server side (a2a runner) chooses the model.
  modelSelector: false,
  toolLabels: TOOL_LABELS,
  renderLink: defaultRenderLink,
};
