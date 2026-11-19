import { fileURLToPath } from 'node:url';

const binary = fileURLToPath(new URL('./native/forge-linux-amd64', import.meta.url));
process.execve!(binary, [binary, '--local'], process.env);
