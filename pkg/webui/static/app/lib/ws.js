let ws = null;
const handlers = {};

function dispatch(type, data) {
  (handlers[type] || []).forEach(fn => fn(data));
}

export function wsOn(type, fn) {
  if (!handlers[type]) handlers[type] = [];
  handlers[type].push(fn);
  return () => { handlers[type] = handlers[type].filter(f => f !== fn); };
}

export function wsSend(type, data) {
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ type, data }));
  }
}

export function wsConnect() {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  ws = new WebSocket(`${proto}://${location.host}/ws`);
  ws.onopen = () => {
    wsSend('sub:logs');
    dispatch('ws:status', { connected: true });
  };
  ws.onmessage = (e) => {
    try {
      const msg = JSON.parse(e.data);
      dispatch(msg.type, msg.data);
    } catch(_) {}
  };
  ws.onclose = () => {
    ws = null;
    dispatch('ws:status', { connected: false });
    setTimeout(wsConnect, 3000);
  };
  ws.onerror = () => { if (ws) ws.close(); };
}
