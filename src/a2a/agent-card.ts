// src/a2a/agent-card.ts
//
// Builds the A2A v1.0 AgentCard served at
// /.well-known/agent-card.json. UNSIGNED for v0 (no `signatures`) — card
// signing is a documented follow-up in the README.
import type { Config } from '../config';

export interface AgentSkill {
  id: string;
  name: string;
  description: string;
  tags: string[];
  examples?: string[];
  inputModes?: string[];
  outputModes?: string[];
}

export interface AgentCard {
  protocolVersion: string;
  name: string;
  description: string;
  /** The JSON-RPC endpoint clients POST to. */
  url: string;
  /** Transport at `url`. A2A defines "JSONRPC" | "GRPC" | "HTTP+JSON". */
  preferredTransport: string;
  version: string;
  provider: {
    organization: string;
    url: string;
  };
  capabilities: {
    streaming: boolean;
    pushNotifications: boolean;
    stateTransitionHistory: boolean;
  };
  defaultInputModes: string[];
  defaultOutputModes: string[];
  securitySchemes: Record<string, unknown>;
  security: Array<Record<string, string[]>>;
  skills: AgentSkill[];
}

export const AGENT_VERSION = '0.1.0';

export function buildAgentCard(config: Config): AgentCard {
  return {
    protocolVersion: '1.0',
    name: 'Patch',
    description:
      'Patch is the Datum Cloud assistant. It answers questions about a project and its ' +
      'resources and can invoke provider service tools that are entitled to the project ' +
      'through the Datum agent framework.',
    url: `${config.publicBaseUrl}/a2a`,
    preferredTransport: 'JSONRPC',
    version: AGENT_VERSION,
    provider: {
      organization: 'Datum',
      url: 'https://www.datum.net',
    },
    capabilities: {
      streaming: true,
      pushNotifications: false,
      stateTransitionHistory: false,
    },
    defaultInputModes: ['text/plain'],
    defaultOutputModes: ['text/plain'],
    securitySchemes: {
      bearer: {
        type: 'http',
        scheme: 'bearer',
        description:
          'Bearer token. In AUTH_MODE=dev, a static token from AUTH_DEV_TOKENS; in ' +
          'AUTH_MODE=oidc, a JWT from the configured OIDC issuer.',
      },
    },
    security: [{ bearer: [] }],
    skills: [
      {
        id: 'project-assistant',
        name: 'Project assistant',
        description:
          'General assistance for a Datum Cloud project: answering questions about the ' +
          'project and its resources, and running entitled provider service tools (for ' +
          'example, diagnosing a provider pipeline).',
        tags: ['datum', 'assistant', 'project', 'agent-framework'],
        examples: [
          'Diagnose pipeline p-1 for StreamCo',
          'What can you help me with in this project?',
        ],
        inputModes: ['text/plain'],
        outputModes: ['text/plain'],
      },
    ],
  };
}
