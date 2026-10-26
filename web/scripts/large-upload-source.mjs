#!/usr/bin/env node
// Creates a deterministic real (non-sparse) input, never an acceptance result.
import assert from 'node:assert/strict';
import { open, mkdir, unlink } from 'node:fs/promises';
import { dirname, resolve } from 'node:path';
import { parseArgs } from 'node:util';
import { createHash } from 'node:crypto';
import { expectedSize } from './large-upload-lib.mjs';
const { values: args } = parseArgs({ options: { output: { type: 'string' }, 'smoke-bytes': { type: 'string' } }, strict: true });
assert.ok(args.output, '--output PATH is required; an existing file is never overwritten');
const size = expectedSize(args['smoke-bytes']), path = resolve(args.output);
await mkdir(dirname(path), { recursive: true });
const handle = await open(path, 'wx', 0o600);
let done = false;
try {
  // xorshift32 with a fixed seed; repeated 1 MiB deterministic block, no truncate/sparse trick.
  const block = Buffer.alloc(1024 * 1024); let seed = 0x6d2b79f5;
  for (let i = 0; i < block.length; i += 4) { seed ^= seed << 13; seed ^= seed >>> 17; seed ^= seed << 5; block.writeUInt32LE(seed >>> 0, i); }
  let bytes = 0; const hash = createHash('sha256');
  while (bytes < size) {
    const chunk = block.subarray(0, Math.min(block.length, size - bytes));
    let offset = 0; while (offset < chunk.length) { const result = await handle.write(chunk, offset, chunk.length - offset, bytes + offset); assert.ok(result.bytesWritten > 0); offset += result.bytesWritten; }
    hash.update(chunk); bytes += chunk.length;
  }
  await handle.sync(); assert.equal((await handle.stat()).size, size); done = true;
  console.log(JSON.stringify({ artifact: 'deterministic_upload_input_only', path, bytes, sha256: hash.digest('hex'), generator: 'xorshift32_seed_0x6d2b79f5_repeated_1MiB_non_sparse', acceptance_executed: false, mode: args['smoke-bytes'] === undefined ? 'exact_10gib_input' : 'small_smoke_input_not_10gib' }));
} finally { await handle.close(); if (!done) await unlink(path).catch(() => {}); }
