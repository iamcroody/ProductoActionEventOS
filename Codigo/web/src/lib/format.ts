// Currency + date formatting helpers. COP shown as $1.234.567 (no decimals).

export function cop(value: number | null | undefined): string {
  const n = Math.round(Number(value ?? 0));
  const sign = n < 0 ? '-' : '';
  const abs = Math.abs(n).toString();
  const grouped = abs.replace(/\B(?=(\d{3})+(?!\d))/g, '.');
  return `${sign}$${grouped}`;
}

export function copInput(raw: string): number {
  const digits = raw.replace(/[^\d]/g, '');
  return digits ? parseInt(digits, 10) : 0;
}

// Durations in seconds → "43 s" / "2 m 5 s". Used by closing/timing reports.
export function secs(value: number | null | undefined): string {
  const n = Math.round(Number(value ?? 0));
  if (n <= 0) return '—';
  if (n < 60) return `${n} s`;
  const m = Math.floor(n / 60);
  const r = n % 60;
  return r ? `${m} m ${r} s` : `${m} m`;
}

export function dateTime(ms: number | null | undefined): string {
  if (!ms) return '—';
  const d = new Date(ms);
  return d.toLocaleString('es-CO', {
    day: '2-digit',
    month: '2-digit',
    year: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  });
}

export function timeOnly(ms: number | null | undefined): string {
  if (!ms) return '—';
  const d = new Date(ms);
  return d.toLocaleTimeString('es-CO', { hour: '2-digit', minute: '2-digit' });
}

export function relTime(ms: number | null | undefined): string {
  if (!ms) return '';
  const diff = Date.now() - ms;
  const m = Math.floor(diff / 60000);
  if (m < 1) return 'ahora';
  if (m < 60) return `${m} min`;
  const h = Math.floor(m / 60);
  return `${h} h`;
}
