import { ComponentChildren } from 'preact';
import { useEffect } from 'preact/hooks';
import { useToasts } from '../lib/toast';

export function Spinner() {
  return <span class="spinner" aria-label="Cargando" />;
}

export function Loading({ label = 'Cargando…' }: { label?: string }) {
  return (
    <div class="state">
      <Spinner />
      <div style="margin-top:12px">{label}</div>
    </div>
  );
}

export function EmptyState({ icon = '◦', title, hint }: { icon?: string; title: string; hint?: string }) {
  return (
    <div class="state">
      <div class="big">{icon}</div>
      <div style="font-weight:600;color:var(--text)">{title}</div>
      {hint && <div style="margin-top:6px;font-size:13.5px">{hint}</div>}
    </div>
  );
}

export function ErrorState({ message, onRetry }: { message: string; onRetry?: () => void }) {
  return (
    <div class="state">
      <div class="big" style="color:var(--red)">!</div>
      <div style="font-weight:600;color:var(--text)">{message}</div>
      {onRetry && (
        <button class="btn sm" style="margin-top:14px" onClick={onRetry}>Reintentar</button>
      )}
    </div>
  );
}

export function Modal({ title, children, onClose, footer, wide }: {
  title: string;
  children: ComponentChildren;
  onClose: () => void;
  footer?: ComponentChildren;
  wide?: boolean;
}) {
  // ESC closes the topmost modal (matches the global "cancelar/cerrar" shortcut).
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose(); };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [onClose]);

  return (
    <div class="modal-back" onClick={onClose}>
      <div class="modal" style={wide ? 'width:min(760px,100%)' : ''} onClick={(e) => e.stopPropagation()}>
        <div class="modal-head">
          <h3>{title}</h3>
          <button class="icon-btn" onClick={onClose} aria-label="Cerrar">×</button>
        </div>
        <div class="modal-body">{children}</div>
        {footer && <div class="modal-foot">{footer}</div>}
      </div>
    </div>
  );
}

export function Toasts() {
  const toasts = useToasts();
  if (!toasts.length) return null;
  return (
    <div class="toasts" aria-live="polite">
      {toasts.map((t) => (
        <div key={t.id} class={`toast ${t.kind}`}>{t.message}</div>
      ))}
    </div>
  );
}

export function Field({ label, children }: { label: string; children: ComponentChildren }) {
  return (
    <div class="field">
      <label>{label}</label>
      {children}
    </div>
  );
}
