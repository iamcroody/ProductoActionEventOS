// Minimal global auth + toast stores using a subscribe pattern.
import { useEffect, useState } from 'preact/hooks';
import { Auth } from './api';
import type { Role, User } from './types';

type AuthState = {
  user: User | null;
  loading: boolean;
};

let state: AuthState = { user: null, loading: true };
const subs = new Set<() => void>();
function emit() { subs.forEach((f) => f()); }
function set(patch: Partial<AuthState>) { state = { ...state, ...patch }; emit(); }

export async function bootstrapAuth() {
  try {
    const user = await Auth.me();
    set({ user, loading: false });
  } catch {
    set({ user: null, loading: false });
  }
}

export async function doLogin(username: string, secret: string): Promise<User> {
  await Auth.login(username, secret);
  const user = await Auth.me();
  set({ user, loading: false });
  return user;
}

export async function doLogout() {
  try { await Auth.logout(); } catch { /* ignore */ }
  set({ user: null });
}

export function clearSession() { set({ user: null }); }

export function useAuth(): AuthState {
  const [, force] = useState(0);
  useEffect(() => {
    const f = () => force((n) => n + 1);
    subs.add(f);
    return () => { subs.delete(f); };
  }, []);
  return state;
}

// Role → which sections are visible.
export const ROLE_HOME: Record<Role, string> = {
  admin: '/dashboard',
  caja: '/pos',
  mesero: '/mesero',
  cocina: '/pendientes',
  bar: '/pendientes',
  auditor: '/reportes',
};

export function canAccess(role: Role | undefined, section: string): boolean {
  if (!role) return false;
  if (role === 'admin') return true;
  const map: Record<string, Role[]> = {
    dashboard: ['caja', 'auditor'],
    pos: ['caja'],
    caja: ['caja'],
    mesero: ['mesero'],
    pendientes: ['cocina', 'bar', 'mesero'],
    asistentes: ['caja'],
    acceso: ['caja'],
    reportes: ['caja', 'auditor'],
    cierre: [],
    auditoria: ['auditor'],
    monitor: [],
    admin: [],
  };
  return (map[section] ?? []).includes(role);
}
