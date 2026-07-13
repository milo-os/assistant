// LIFTED VERBATIM from cloud-portal
//   branch:  feat/patch-dynamic-composition
//   path:    app/modules/assistant/composition/fixture-source.test.ts
// -----------------------------------------------------------------------
import { FixtureAgentBindingSource } from './fixture-source';
import type { CompositionLogger } from './types';
import { afterAll, describe, expect, it, mock } from 'bun:test';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const dir = mkdtempSync(join(tmpdir(), 'agent-bindings-fixture-'));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

let fileCounter = 0;
function writeFixture(content: string): string {
  const path = join(dir, `fixture-${fileCounter++}.json`);
  writeFileSync(path, content);
  return path;
}

const validBinding = {
  apiVersion: 'services.miloapis.com/v1alpha1',
  kind: 'AgentBinding',
  metadata: { name: 'streamco-binding', namespace: 'demo-project' },
  spec: {
    serviceRef: { name: 'streamco' },
    serviceName: 'streaming.streamco.example',
    serviceAgentRef: { name: 'streamco-agent' },
    configurationVersion: 'v1',
    knowledge: {
      sources: [{ type: 'LLMDocs', title: 'Overview', url: 'http://127.0.0.1:7810/llms-full.txt' }],
      concepts: [
        { gvk: { group: 'streaming.streamco.example', kind: 'Stream' }, summary: 'A live stream' },
      ],
    },
    tools: {
      mcpServers: [
        {
          name: 'streamco',
          endpoint: 'http://127.0.0.1:7810/mcp',
          toolSelector: { include: ['streams_list', 'streams_get', 'pipeline_diagnose'] },
          mutating: [],
        },
      ],
    },
    authority: {
      reads: [{ gvk: { group: 'streaming.streamco.example', kind: '*' } }],
      maxTaskDurationSeconds: 60,
    },
  },
  status: { conditions: [{ type: 'Ready', status: 'True' }] },
};

describe('FixtureAgentBindingSource', () => {
  it('parses a bare array of AgentBinding objects (kubectl ... | jq .items form)', async () => {
    const path = writeFixture(JSON.stringify([validBinding]));
    const bindings = await new FixtureAgentBindingSource(path).getBindings('demo-project');

    expect(bindings).toHaveLength(1);
    const binding = bindings[0]!;
    expect(binding.spec.serviceName).toBe('streaming.streamco.example');
    expect(binding.spec.serviceRef.name).toBe('streamco');
    expect(binding.spec.configurationVersion).toBe('v1');
    expect(binding.spec.tools?.mcpServers[0]?.toolSelector.include).toEqual([
      'streams_list',
      'streams_get',
      'pipeline_diagnose',
    ]);
    expect(binding.spec.knowledge?.sources[0]?.type).toBe('LLMDocs');
    expect(binding.spec.authority?.maxTaskDurationSeconds).toBe(60);
  });

  it('accepts the full List object form ({items: [...]})', async () => {
    const path = writeFixture(JSON.stringify({ kind: 'List', items: [validBinding] }));
    const bindings = await new FixtureAgentBindingSource(path).getBindings('demo-project');
    expect(bindings).toHaveLength(1);
    expect(bindings[0]!.spec.serviceName).toBe('streaming.streamco.example');
  });

  it('applies CRD-shaped defaults for omitted list fields', async () => {
    const minimal = {
      spec: {
        serviceRef: { name: 's' },
        serviceName: 'svc.example.com',
        serviceAgentRef: { name: 'a' },
        configurationVersion: 'v1',
        tools: {
          mcpServers: [{ name: 'srv', endpoint: 'http://x/mcp', toolSelector: { include: ['t'] } }],
        },
      },
    };
    const path = writeFixture(JSON.stringify([minimal]));
    const bindings = await new FixtureAgentBindingSource(path).getBindings('p');
    expect(bindings[0]!.spec.tools?.mcpServers[0]?.mutating).toEqual([]);
    expect(bindings[0]!.spec.knowledge).toBeUndefined();
  });

  it('skips entries that fail schema validation and keeps the valid ones', async () => {
    const invalid = { spec: { serviceName: 'missing-required-fields.example' } };
    const path = writeFixture(JSON.stringify([invalid, validBinding]));
    const warn = mock(() => {});
    const logger: CompositionLogger = { info: () => {}, warn };

    const bindings = await new FixtureAgentBindingSource(path, { logger }).getBindings('p');

    expect(bindings).toHaveLength(1);
    expect(bindings[0]!.spec.serviceName).toBe('streaming.streamco.example');
    expect(warn).toHaveBeenCalledTimes(1);
  });

  it('throws on invalid JSON', async () => {
    const path = writeFixture('{not json');
    await expect(new FixtureAgentBindingSource(path).getBindings('p')).rejects.toThrow(
      /not valid JSON/
    );
  });

  it('throws when the root is neither an array nor a List object', async () => {
    const path = writeFixture(JSON.stringify({ spec: {} }));
    await expect(new FixtureAgentBindingSource(path).getBindings('p')).rejects.toThrow(
      /must be a JSON array/
    );
  });

  it('propagates a missing-file error', async () => {
    const source = new FixtureAgentBindingSource(join(dir, 'does-not-exist.json'));
    await expect(source.getBindings('p')).rejects.toThrow();
  });
});
