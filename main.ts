// The entry <-> env lifecycle: bind nothing here, hand the whole process over to the forge binary.
//
// main.ts is the contest's entrypoint (`node /bot/main.ts` in the grading pod). The forge itself is
// the Go server under cmd/forge, built to a static Linux binary at ./forge and committed beside this
// file. This file only chooses the right build for the host, starts it with the env inherited, and
// forwards its exit code, so the env sees exactly one process speaking HTTP.
//
//   1. On win32 the local loop runs ./forge.exe; everywhere else (the grading Linux pod) it runs
//      ./forge. Both come from the same Go source.
//   2. The path is resolved from import.meta.dirname, never from process.cwd(): the local process
//      driver imports this file from its own launcher, so the cwd is not the repository.
//   3. MAKEFUN_ENV_URL is read by the binary, not here; it is inherited unchanged.
//   4. A missing or unstartable binary prints the spawn error on stderr and exits 1. Nothing is
//      swallowed: the trial must never see a green run backed by a process that does not exist.
//   5. When the binary exits (the env said the trial is over), this process exits with the same code.

import { spawn } from 'node:child_process';
import { join } from 'node:path';

const binary = join(import.meta.dirname, process.platform === 'win32' ? 'forge.exe' : 'forge');

const child = spawn(binary, [], { stdio: 'inherit', env: process.env });

child.on('error', (err: Error) => {
  process.stderr.write(`[entry] could not start ${binary}: ${err.message}\n`);
  process.exit(1);
});

child.on('exit', (code: number | null, signal: NodeJS.Signals | null) => {
  if (signal !== null) {
    process.stderr.write(`[entry] ${binary} was killed by ${signal}\n`);
    process.exit(1);
  }
  process.exit(code ?? 1);
});
