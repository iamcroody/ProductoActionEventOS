import { useEffect } from 'preact/hooks';
import { LocationProvider, Router, Route, useLocation } from 'preact-iso';
import { bootstrapAuth, clearSession, useAuth, ROLE_HOME } from './lib/auth';
import { Toasts, Loading } from './components/ui';
import { Shell } from './components/Shell';
import { Login } from './screens/Login';
import { Pos } from './screens/Pos';
import { Mesero } from './screens/Mesero';
import { Pendientes } from './screens/Pendientes';
import { Dashboard } from './screens/Dashboard';
import { Reports } from './screens/Reports';
import { Audit } from './screens/Audit';
import { Admin } from './screens/Admin';
import { Monitor } from './screens/Monitor';
import { Attendees } from './screens/Attendees';
import { ClientAccess } from './screens/ClientAccess';
import { Closing } from './screens/Closing';
import { Spinner } from './components/ui';

function Redirect({ to }: { to: string }) {
  const loc = useLocation();
  useEffect(() => { loc.route(to, true); }, [to]);
  return <div class="center-screen"><Spinner /></div>;
}

function Protected() {
  const { user, loading } = useAuth();
  const loc = useLocation();

  if (loading) return <div class="center-screen"><Loading /></div>;
  if (!user) return <Login />;

  return (
    <Shell>
      <Router>
        <Route path="/" component={() => <Redirect to={ROLE_HOME[user.Role]} />} />
        <Route path="/pos" component={Pos} />
        <Route path="/mesero" component={Mesero} />
        <Route path="/pendientes" component={Pendientes} />
        <Route path="/dashboard" component={Dashboard} />
        <Route path="/reportes" component={Reports} />
        <Route path="/auditoria" component={Audit} />
        <Route path="/asistentes" component={Attendees} />
        <Route path="/acceso" component={ClientAccess} />
        <Route path="/cierre" component={Closing} />
        <Route path="/monitor" component={Monitor} />
        <Route path="/admin" component={Admin} />
        <Route path="/admin/:tab" component={Admin} />
        <Route default component={() => <Redirect to={ROLE_HOME[user.Role]} />} />
      </Router>
    </Shell>
  );
}

export function App() {
  useEffect(() => {
    bootstrapAuth();
    const onUnauth = () => clearSession();
    window.addEventListener('aeos:unauthorized', onUnauth);
    return () => window.removeEventListener('aeos:unauthorized', onUnauth);
  }, []);

  return (
    <LocationProvider>
      <Protected />
      <Toasts />
    </LocationProvider>
  );
}
