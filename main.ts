// Entry point for the make.fun agent/v1 harness.
//
// The forge itself is a single static Go binary (./forge, committed next to
// this file) that serves git smart HTTP, REST v3, GraphQL and MCP from one
// process. This launcher only starts it: the binary binds an ephemeral port,
// registers it with $MAKEFUN_ENV_URL and polls /done itself, exiting 0 when
// the trial ends. The launcher forwards signals and mirrors the exit code.

import { spawn } from 'node:child_process';
import { chmodSync, copyFileSync, existsSync, mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const committed = join(here, 'forge');

if (!existsSync(committed)) {
  process.stderr.write('[entry] forge binary is missing next to main.ts\n');
  process.exit(1);
}

// The grading pod may mount the entry read-only with the binary's exec bit
// cleared (a web-uploaded repo flattens modes to 0644): chmod on the
// committed file then fails and spawning it dies with EACCES. Always run a
// private copy from a writable temp dir instead.
let binary = committed;
try {
  const dir = mkdtempSync(join(tmpdir(), 'forge-'));
  const copy = join(dir, 'forge');
  copyFileSync(committed, copy);
  chmodSync(copy, 0o755);
  binary = copy;
} catch {
  try {
    chmodSync(committed, 0o755);
  } catch {
    // last resort: spawn the committed path and let the error surface
  }
}

const child = spawn(binary, [], { stdio: 'inherit', env: process.env });

for (const signal of ['SIGTERM', 'SIGINT'] as const) {
  process.on(signal, () => {
    try {
      child.kill(signal);
    } catch {
      // child already gone
    }
  });
}

child.on('error', (err) => {
  process.stderr.write(`[entry] failed to start forge: ${err.message}\n`);
  process.exit(1);
});

child.on('exit', (code, signal) => {
  if (signal) {
    process.exit(0);
    return;
  }
  process.exit(code ?? 0);
});
