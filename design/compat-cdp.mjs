// Local NVM Node controller. One request per fresh ownership proof; no packages.
// HOST CHECKER ROOT REMOTE_PORT list
// HOST CHECKER ROOT REMOTE_PORT TARGET_ID request.json
import {execFileSync, spawn} from 'node:child_process';
import {readFileSync} from 'node:fs';
import net from 'node:net';
import {once} from 'node:events';
import {setTimeout as delay} from 'node:timers/promises';
import {deepStrictEqual} from 'node:assert';

const [host, checker, root, remotePort, targetID, requestFile] = process.argv.slice(2);
if (!host || !checker || !/^\/tmp\/appclone-w4-[^/]+$/.test(root || '') || !/^\d+$/.test(remotePort || '')) {
  throw new Error('HOST CHECKER ROOT REMOTE_PORT list|TARGET_ID [request.json]');
}
const quote = value => "'" + value.replaceAll("'", "'\\''") + "'";
const sshOptions = ['-o', 'BatchMode=yes', '-o', 'ConnectTimeout=5'];
const prove = () => JSON.parse(execFileSync('ssh', [...sshOptions, host,
  ['bash', checker, root, remotePort].map(quote).join(' ')], {encoding:'utf8', timeout:8000}));
const identity = prove();
const recheck = () => {
  if (JSON.stringify(prove()) !== JSON.stringify(identity)) throw new Error('diagnostic process identity changed');
};
const reserve = net.createServer();
reserve.listen(0, '127.0.0.1');
await once(reserve, 'listening');
const localPort = reserve.address().port;
await new Promise(resolve => reserve.close(resolve));
const tunnel = spawn('ssh', [...sshOptions, '-o', 'ExitOnForwardFailure=yes', '-N',
  '-L', `127.0.0.1:${localPort}:127.0.0.1:${remotePort}`, host], {stdio:['ignore','ignore','pipe']});
let tunnelError = '';
tunnel.stderr.on('data', data => { tunnelError = (tunnelError + data).slice(-4000); });
let ws;
try {
  let targets;
  for (let attempt = 0; attempt < 40; attempt++) {
    if (tunnel.exitCode !== null) throw new Error(`SSH forwarding failed: ${tunnelError}`);
    try {
      const response = await fetch(`http://127.0.0.1:${localPort}/json/list`, {signal:AbortSignal.timeout(500)});
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      targets = await response.json();
      break;
    } catch { await delay(100); }
  }
  if (!targets) throw new Error('diagnostic endpoint unavailable');
  recheck();
  if (targetID === 'list') {
    console.log(JSON.stringify({identity, targets:targets.map(({id,type,title,url}) => ({id,type,title,url}))}, null, 2));
  } else {
    const target = targets.find(item => item.id === targetID);
    if (!target) throw new Error('target missing; take a new list snapshot');
    const remoteWS = new URL(target.webSocketDebuggerUrl);
    if (remoteWS.protocol !== 'ws:' || !['127.0.0.1','localhost'].includes(remoteWS.hostname) ||
        ![remotePort, String(localPort)].includes(remoteWS.port)) {
      throw new Error('unexpected diagnostic websocket address');
    }
    remoteWS.hostname = '127.0.0.1'; remoteWS.port = String(localPort);
    ws = new WebSocket(remoteWS);
    await Promise.race([once(ws, 'open'), delay(5000).then(() => { throw new Error('websocket timeout'); })]);
    let nextID = 0;
    async function send(method, params) {
      recheck();
      const id = ++nextID;
      return new Promise((resolve, reject) => {
        const timer = setTimeout(() => { ws.removeEventListener('message', receive); reject(new Error('CDP request timeout')); }, 10000);
        function receive(event) {
          const data = JSON.parse(event.data);
          if (data.id !== id) return;
          clearTimeout(timer); ws.removeEventListener('message', receive);
          if (data.error || data.result?.exceptionDetails) {
            reject(new Error(JSON.stringify(data.error || data.result.exceptionDetails)));
          } else resolve(data.result);
        }
        ws.addEventListener('message', receive);
        ws.send(JSON.stringify({id, method, params}));
      });
    }
    const request = JSON.parse(readFileSync(requestFile, 'utf8'));
    if (request.main) {
      if (target.type !== 'node') throw new Error('main-process API requires a node inspector target');
      const proof = await send('Runtime.evaluate', {expression:'({pid:process.pid, home:process.env.HOME})', returnByValue:true});
      if (proof.result?.value?.pid !== identity.namespacePID || proof.result?.value?.home !== `${root}/home`) {
        throw new Error('main-process runtime identity mismatch');
      }
    }
    const result = await send(request.method, request.params || {});
    const asserted = Object.hasOwn(request, 'expect');
    if (asserted) deepStrictEqual(result.result?.value, request.expect);
    console.log(JSON.stringify({identity, asserted, result}, null, 2));
  }
} finally {
  ws?.close();
  tunnel.kill('SIGTERM');
  await Promise.race([once(tunnel, 'exit'), delay(3000).then(() => tunnel.kill('SIGKILL'))]);
}
