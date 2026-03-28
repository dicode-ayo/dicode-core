export function esc(str) {
  return String(str || '').replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

export function escAttr(str) {
  return esc(str).replace(/'/g, '&#39;');
}

export function fmtTime(ts) {
  if (!ts) return '—';
  if (typeof ts === 'number') {
    // Unix seconds if value looks like seconds (< year 3000 in ms)
    return new Date(ts < 1e11 ? ts * 1000 : ts).toLocaleString();
  }
  return new Date(ts).toLocaleString();
}

export function fmtDuration(start, end) {
  if (!end) return '—';
  const ms = new Date(end) - new Date(start);
  if (isNaN(ms)) return '—';
  return (ms / 1000).toFixed(1) + 's';
}
