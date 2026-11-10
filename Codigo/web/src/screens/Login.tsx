import { useState } from 'preact/hooks';
import { doLogin } from '../lib/auth';
import { ApiError } from '../lib/api';

const KEYS = ['1', '2', '3', '4', '5', '6', '7', '8', '9', 'clear', '0', 'back'];

export function Login() {
  const [username, setUsername] = useState('');
  const [secret, setSecret] = useState('');
  const [showPad, setShowPad] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const submit = async () => {
    if (!username.trim()) { setError('Ingresa el usuario'); return; }
    if (secret.length < 3) { setError('Ingresa la contraseña o PIN'); return; }
    setBusy(true);
    setError(null);
    try {
      await doLogin(username.trim(), secret);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : 'Usuario o contraseña incorrectos');
      setSecret('');
    } finally {
      setBusy(false);
    }
  };

  // The numeric pad is an optional convenience for cashiers/waiters on phones
  // who log in with a numeric PIN; it appends to the same secret field.
  const press = (k: string) => {
    setError(null);
    if (k === 'clear') { setSecret(''); return; }
    if (k === 'back') { setSecret((p) => p.slice(0, -1)); return; }
    setSecret((p) => (p.length >= 12 ? p : p + k));
  };

  return (
    <div class="login-wrap">
      <form class="login-card" onSubmit={(e) => { e.preventDefault(); submit(); }}>
        <div class="login-brand">
          <div class="brand-mark">A</div>
          <h1>Action <span class="accent">EventOS</span></h1>
          <div class="org">Consola de operación · ABC Corporation</div>
        </div>

        <div class="field" style="margin-bottom:16px">
          <label>Usuario</label>
          <input
            class="input"
            autoComplete="username"
            placeholder="Usuario"
            value={username}
            onInput={(e) => setUsername((e.target as HTMLInputElement).value)}
            autoFocus
          />
        </div>

        <div class="field">
          <label>Contraseña o PIN</label>
          <input
            class="input"
            type="password"
            autoComplete="current-password"
            placeholder="••••••"
            value={secret}
            onInput={(e) => { setError(null); setSecret((e.target as HTMLInputElement).value); }}
          />
          <button
            type="button"
            class="btn ghost sm block"
            style="margin-top:10px"
            onClick={() => setShowPad((s) => !s)}
          >
            {showPad ? 'Ocultar teclado numérico' : 'Usar teclado numérico (PIN)'}
          </button>
          {showPad && (
            <div class="pinpad" style="margin-top:12px">
              {KEYS.map((k) => (
                <button
                  type="button"
                  key={k}
                  class={`pin-key ${k === 'clear' || k === 'back' ? 'action' : ''}`}
                  onClick={() => press(k)}
                >
                  {k === 'clear' ? 'C' : k === 'back' ? '⌫' : k}
                </button>
              ))}
            </div>
          )}
        </div>

        {error && (
          <div class="cash-warn" style="margin-top:16px;background:var(--red-soft);color:var(--red)">
            {error}
          </div>
        )}

        <button class="btn primary lg block" style="margin-top:20px" disabled={busy}>
          {busy ? 'Ingresando…' : 'Ingresar'}
        </button>

        <div class="login-foot">
          <div class="sig">Powered by <b>CROODY</b></div>
        </div>
      </form>
    </div>
  );
}
