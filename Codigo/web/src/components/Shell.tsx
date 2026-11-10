import { ComponentChildren } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import { useLocation } from 'preact-iso';
import { doLogout, useAuth } from '../lib/auth';
import { useHotkeys, requestNewAttendee, focusSearch } from '../lib/hotkeys';
import type { Role } from '../lib/types';

type Section = 'op' | 'gestion';
interface NavItem { path: string; label: string; icon: string; roles: Role[] | '*'; section: Section; live?: boolean; }

// Operator's daily actions come first ("OPERACIÓN"); reporting/admin is grouped
// and visually separated under "GESTIÓN". For caja, "Caja" is the first item.
const NAV: NavItem[] = [
  { path: '/pos', label: 'Caja', icon: '◉', roles: ['admin', 'caja'], section: 'op' },
  { path: '/mesero', label: 'Mesero', icon: '✦', roles: ['admin', 'mesero'], section: 'op' },
  { path: '/pendientes', label: 'Pendientes', icon: '◎', roles: ['admin', 'cocina', 'bar', 'mesero'], section: 'op' },
  { path: '/asistentes', label: 'Asistentes', icon: '☷', roles: ['admin', 'caja'], section: 'op' },
  { path: '/acceso', label: 'Acceso clientes', icon: '◳', roles: ['admin', 'caja'], section: 'op' },
  { path: '/dashboard', label: 'Dashboard', icon: '◧', roles: ['admin', 'caja', 'auditor'], section: 'gestion' },
  { path: '/reportes', label: 'Reportes', icon: '◫', roles: ['admin', 'caja', 'auditor'], section: 'gestion' },
  { path: '/cierre', label: 'Cierre', icon: '◰', roles: ['admin'], section: 'gestion' },
  { path: '/auditoria', label: 'Auditoría', icon: '◈', roles: ['admin', 'auditor'], section: 'gestion' },
  { path: '/monitor', label: 'Red en vivo', icon: '◢', roles: ['admin'], section: 'gestion', live: true },
  { path: '/admin', label: 'Administración', icon: '⚙', roles: ['admin'], section: 'gestion' },
];

const SECTION_LABEL: Record<Section, string> = { op: 'Operación', gestion: 'Gestión' };
const SECTION_ORDER: Section[] = ['op', 'gestion'];

function initials(name: string): string {
  const parts = name.trim().split(/\s+/);
  return ((parts[0]?.[0] ?? '') + (parts[1]?.[0] ?? '')).toUpperCase() || '?';
}

export function Shell({ children }: { children: ComponentChildren }) {
  const { user } = useAuth();
  const loc = useLocation();
  const [drawer, setDrawer] = useState(false);

  // While the mobile drawer is open, lock the page behind it and let Escape
  // close it — small touches that make the phone experience feel native.
  useEffect(() => {
    if (!drawer) return;
    const prev = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setDrawer(false); };
    window.addEventListener('keydown', onKey);
    return () => {
      document.body.style.overflow = prev;
      window.removeEventListener('keydown', onKey);
    };
  }, [drawer]);

  // Global shortcuts available on every screen. F3 (cobrar) and Escape's
  // "limpiar selección" are handled where the cart/selection lives (Caja).
  useHotkeys({
    F1: () => {
      requestNewAttendee();
      if (!loc.path.startsWith('/asistentes')) loc.route('/asistentes');
    },
    F2: () => focusSearch(),
  });

  if (!user) return null;

  const items = NAV.filter((n) => n.roles === '*' || n.roles.includes(user.Role));
  const current = items.find((n) => loc.path.startsWith(n.path));

  const go = (path: string) => { loc.route(path); setDrawer(false); };

  const SidebarInner = (
    <>
      <div class="brand">
        <div class="brand-mark">A</div>
        <div>
          <div class="brand-name">Action <span class="accent">EventOS</span></div>
          <div class="brand-sub">ABC Corporation</div>
        </div>
      </div>
      <nav class="nav">
        {SECTION_ORDER.map((section) => {
          const group = items.filter((n) => n.section === section);
          if (group.length === 0) return null;
          return (
            <div class="nav-group" key={section}>
              <div class="nav-section">{SECTION_LABEL[section]}</div>
              {group.map((n) => (
                <a
                  key={n.path}
                  class={`nav-link ${loc.path.startsWith(n.path) ? 'active' : ''}`}
                  href={n.path}
                  onClick={(e) => { e.preventDefault(); go(n.path); }}
                >
                  <span class="ico">{n.icon}</span>
                  {n.label}
                  {n.live && <span class="live-dot" aria-hidden="true" />}
                </a>
              ))}
            </div>
          );
        })}
      </nav>
      <div class="side-foot">
        <div class="user-chip">
          <div class="avatar">{initials(user.FullName || user.Username)}</div>
          <div class="grow">
            <div class="user-name">{user.FullName || user.Username}</div>
            <div class="user-role">{user.Role}</div>
          </div>
        </div>
        <button class="btn ghost sm block" style="margin-top:8px" onClick={() => doLogout()}>
          Cerrar sesión
        </button>
        <div class="kbd-hint">
          Atajos <kbd>F1</kbd>nuevo <kbd>F2</kbd>buscar <kbd>F3</kbd>cobrar <kbd>Esc</kbd>cerrar
        </div>
        <div class="sig">Powered by <b>CROODY</b></div>
      </div>
    </>
  );

  return (
    <div class="shell">
      <aside class="sidebar">{SidebarInner}</aside>

      {drawer && (
        <>
          <div class="drawer-back" onClick={() => setDrawer(false)} />
          <aside class="sidebar drawer">{SidebarInner}</aside>
        </>
      )}

      <div class="main">
        <div class="topbar">
          <button class="icon-btn" onClick={() => setDrawer(true)} aria-label="Menú">☰</button>
          <div class="brand-name" style="font-size:15px">Action <span class="accent">EventOS</span></div>
          <div class="right faint" style="font-size:13px">{current?.label ?? ''}</div>
        </div>
        <main class="content">{children}</main>
      </div>
    </div>
  );
}
