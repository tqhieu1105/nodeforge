import { createApp } from './app.ts';

const server = createApp();
server.listen(0, '127.0.0.1', () => {
  const address = server.address();
  if (!address || typeof address === 'string') throw new Error('Expected a TCP port');
  process.stdout.write(`${address.port}\n`);
});
