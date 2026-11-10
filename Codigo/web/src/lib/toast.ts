import { useEffect, useState } from 'preact/hooks';

export interface Toast {
  id: number;
  kind: 'success' | 'error' | 'info';
  message: string;
}

let toasts: Toast[] = [];
let seq = 1;
const subs = new Set<() => void>();
function emit() { subs.forEach((f) => f()); }

export function pushToast(kind: Toast['kind'], message: string) {
  const id = seq++;
  toasts = [...toasts, { id, kind, message }];
  emit();
  setTimeout(() => {
    toasts = toasts.filter((t) => t.id !== id);
    emit();
  }, kind === 'error' ? 5000 : 2600);
}

export const toast = {
  ok: (m: string) => pushToast('success', m),
  err: (m: string) => pushToast('error', m),
  info: (m: string) => pushToast('info', m),
};

export function useToasts(): Toast[] {
  const [, force] = useState(0);
  useEffect(() => {
    const f = () => force((n) => n + 1);
    subs.add(f);
    return () => { subs.delete(f); };
  }, []);
  return toasts;
}
