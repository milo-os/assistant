// src/composition/control-plane-source.ts
//
// LIFTED VERBATIM from cloud-portal
//   branch:  feat/patch-dynamic-composition
//   path:    app/modules/assistant/composition/control-plane-source.ts
// Still a stub — see the standalone-service README "Follow-ups" for the
// control-plane binding source work item.
// -----------------------------------------------------------------------
//
// Production implementation of AgentCapabilitySource: list AgentBinding
// objects (services.miloapis.com/v1alpha1) projected into the consumer
// project's scope on the Milo control plane.
//
// TODO(agent-framework): implement once the AgentBinding API is served
// by the control plane. Sketch:
//   GET {apiUrl}/apis/services.miloapis.com/v1alpha1/projects/{projectName}/agentbindings
//   with the caller's access token, parse `.items` through
//   agentBindingSchema (same validation path as the fixture source),
//   and optionally filter on status.conditions Ready=True.
// Keep this class env-free: apiUrl and token are injected by the caller.
import type { AgentBinding, AgentCapabilitySource } from './types';

export interface ControlPlaneAgentBindingSourceOptions {
  /** Control-plane API base URL. */
  apiUrl: string;
  /** Caller's access token; bindings are read with the user's identity. */
  accessToken?: string;
}

export class ControlPlaneAgentBindingSource implements AgentCapabilitySource {
  constructor(_options: ControlPlaneAgentBindingSourceOptions) {}

  getBindings(_projectName: string): Promise<AgentBinding[]> {
    // TODO(agent-framework): replace with the control-plane list call
    // described in the file header.
    return Promise.reject(
      new Error(
        'ControlPlaneAgentBindingSource is not implemented yet — use FixtureAgentBindingSource (AGENT_BINDINGS_FIXTURE) for the local slice'
      )
    );
  }
}
