import { fileURLToPath } from 'node:url';

// Replace the launcher so the forge still runs as one process in the trial.
const binary = fileURLToPath(new URL('./native/forge-linux-amd64', import.meta.url));
process.execve!(binary, [binary], process.env);
