// Thin launcher: the entrypoint the harness runs as `node main.ts`.
// All forge logic lives in the statically linked Go binary `forge`
// (committed next to this file with its .go sources); this file only
// spawns it, forwards stdio/env, and exits with its code. Keeping the
// launcher in TypeScript is what lets the harness's Node entrypoint
// (`node main.ts`) start the Go core without any change to how the
// competition launches the entry.
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const dir = path.dirname(fileURLToPath(import.meta.url));
const binary = path.join(dir, 'forge');

const child = spawn(binary, [], { stdio: 'inherit', env: process.env });
child.on('error', (err) => {
  process.stderr.write(`[entry] failed to start forge: ${err.message}\n`);
  process.exit(1);
});
child.on('exit', (code) => {
  process.exit(code ?? 1);
});
