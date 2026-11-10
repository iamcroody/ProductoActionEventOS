// Global keyboard layer. F-keys are captured (preventDefault) everywhere; other
// keys never hijack typing inside inputs (Escape is allowed through so modals and
// selections can be cleared while a field is focused).
import { useEffect, useRef } from 'preact/hooks';

type Handler = (e: KeyboardEvent) => void;

export function useHotkeys(handlers: Record<string, Handler>) {
  const ref = useRef(handlers);
  ref.current = handlers;
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const h = ref.current[e.key];
      if (!h) return;
      const isFKey = /^F\d+$/.test(e.key);
      const t = e.target as HTMLElement | null;
      const typing =
        !!t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable);
      if (typing && !isFKey && e.key !== 'Escape') return;
      if (isFKey) e.preventDefault();
      h(e);
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);
}

// F1 can be pressed from any screen; the Asistentes screen reads this flag on
// mount (after the F1 navigation) and also listens live for the event so the
// "Nuevo asistente" form opens immediately.
let pendingNew = false;
export function requestNewAttendee() {
  pendingNew = true;
  window.dispatchEvent(new CustomEvent('aeos:new-attendee'));
}
export function consumeNewAttendee(): boolean {
  const v = pendingNew;
  pendingNew = false;
  return v;
}

// F2 → focus the most relevant search box on the current screen.
export function focusSearch() {
  const el = document.querySelector<HTMLInputElement>('[data-hotkey-search]');
  if (el) {
    el.focus();
    el.select?.();
  }
}
