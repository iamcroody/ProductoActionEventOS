# Action EventOS

**Consola de operación de eventos — offline, un ejecutable, sin filas, sin errores de saldo.**

> *"No desarrollamos software. Diseñamos tecnología que mejora la experiencia humana."*
> — CROODY

---

## El problema que resuelve

En eventos corporativos, gestionar consumos con hojas de cálculo o sistemas en la nube genera tres cuellos de botella críticos:

- **Búsqueda lenta.** Encontrar a un asistente entre cientos de registros toma tiempo; la fila crece.
- **Errores de saldo.** Cuando dos operadores actúan sobre el mismo asistente sin coordinación, el saldo queda inconsistente.
- **Dependencia de internet.** Un evento en una carpa, un parqueadero o una locación con señal débil deja sin sistema a toda la operación.

Action EventOS elimina los tres problemas: corre completamente en un equipo local, atiende a todos los dispositivos de la red Wi-Fi interna, y garantiza integridad de saldo e inventario mediante una arquitectura de escritura serializada.

---

## Propuesta y demo

- Propuesta ejecutiva para ABC Corporation: **[docs/PROPUESTA.md](docs/PROPUESTA.md)**
- Guion de demostración (~3 minutos): **[docs/DEMO.md](docs/DEMO.md)**

---

## Cómo ejecutar

### Operadores de evento

1. Copiar la carpeta del sistema al equipo del evento.
2. Ejecutar `ActionEventOS.exe` (Windows) — o el binario correspondiente en macOS.
3. Al iniciar, el equipo levanta **dos servicios aislados**:
   - **Gestión (staff)** en `http://localhost:8080` — consola de administración y API de operación. Se abre automáticamente en el navegador del equipo.
   - **Clientes (QR)** en el puerto `8090` — únicamente la web del cliente para que los asistentes pidan desde su celular.
4. La terminal imprime **dos códigos QR**: uno **STAFF (privado)** y uno **CLIENTES (público)**. Cada dispositivo escanea el que corresponde a su rol.

No se requiere ninguna configuración adicional. Sin internet. Sin instalar nada.

### Credenciales de primer arranque

| Campo | Valor |
|---|---|
| Usuario | `action` |
| Contraseña | `croody` |

Por seguridad, la pantalla de inicio de sesión **no muestra** estas credenciales: se documentan aquí para el primer arranque. **Cámbialas en Administración → Operadores antes de iniciar la operación.**

### Opciones de línea de comandos

| Flag | Defecto | Descripción |
|---|---|---|
| `-port` | `8080` | Puerto de la consola de gestión (staff). |
| `-client-port` | `8090` | Puerto de la web de clientes (QR), aislado de gestión. |
| `-data` | `data` | Carpeta de la base de datos y los respaldos. |
| `-open` | `true` | Abrir automáticamente el navegador al iniciar. |
| `-admin-key` | *(aleatoria)* | Llave de acceso a gestión. Si se omite, se genera una por arranque. |

---

## Modelo de acceso

Action EventOS separa por completo a quién opera el evento de quién consume en él:

- **Gestión privada con llave.** El puerto de gestión es un *agujero negro* (responde 404) para cualquier dispositivo de la red que no presente la llave de acceso. El equipo anfitrión (loopback) siempre entra. El personal accede desde su celular con un **QR de staff privado** que transporta la llave; una vez validado, una cookie `Strict`/`HttpOnly` la conserva. Sin la llave, la consola es **invisible**: no hay página de login que atacar.
- **Clientes por QR público.** Los asistentes escanean el **QR de clientes** (puerto separado `8090`), que solo sirve la web del cliente y `/api/cliente/*`. Desde ahí no se puede alcanzar la consola ni la API de operación, ni siquiera adivinando rutas.
- **Dos puertos aislados.** Gestión (`8080`) y clientes (`8090`) son servidores independientes. La superficie de gestión nunca se expone en el puerto público.

Ambos QR se imprimen en el banner de la terminal al arrancar y están disponibles dentro de la app en **Acceso clientes** (pantalla *Accesos*).

---

## Características principales

| Módulo | Funcionalidad |
|---|---|
| **Eventos** | Creación, activación (candado) y cierre con respaldo automático |
| **Menú** | Categorías, productos con precio, costo al por mayor y control de stock |
| **Asistentes** | Registro individual o masivo; saldo prepago redimible o sin-redimible; QR por asistente |
| **Saldo y ledger** | Recarga, débito y consulta; ledger inmutable encadenado por hash |
| **Caja** | Apertura, registro de ventas (saldo + efectivo combinado), cierre con diferencial |
| **Auto-pedido QR** | El asistente pide desde su celular; valida identidad; saldo se descuenta automáticamente |
| **Pendientes** | Entrega parcial por ítem (pedido / entregado / faltante); tablero en vivo vía SSE |
| **Ganancias y propinas** | Dashboard con costo, margen y ganancia por producto; reparto de propinas con top-3 meseros y personal de aseo (+5%) |
| **Reporte de cierre** | Consolidado de negocio del evento, descargable en **XLSX** y **CSV** |
| **Red en vivo** | Monitor de tráfico LAN en tiempo real (dispositivos, peticiones y contadores), solo para `admin` |
| **Auditoría** | Log inmutable con cadena de hash SHA-256; verificación de integridad de saldo e inventario |
| **Respaldos** | Automático al arrancar, periódico (~10 min) con retención, al cerrar el evento y al apagar; verificación SHA-256 |

---

## Estructura de la carpeta entregable

```
ActionEventOS/
├── ActionEventOS.exe        # Binario del servidor (Windows)
├── ActionEventOS            # Binario del servidor (macOS)
├── data/                    # Creada automáticamente al primer arranque
│   ├── eventpos.db          # Base de datos SQLite
│   └── backups/             # Respaldos automáticos y manuales
├── scripts/
│   ├── build.sh             # Script de compilación para desarrolladores
│   └── test.sh              # Script de pruebas
├── docs/
│   ├── PROPUESTA.md         # Propuesta ejecutiva
│   ├── DEMO.md              # Guion de demostración
│   ├── API.md
│   ├── ARQUITECTURA.md
│   ├── MANUAL_OPERADOR.md
│   ├── MANUAL_ADMINISTRADOR.md
│   └── MANUAL_INSTALACION.md
├── CHANGELOG.md
├── LICENCIA.md
└── README.md
```

La carpeta `data/` se crea automáticamente en el mismo directorio desde donde se ejecuta el binario. Contiene la base de datos y todos los respaldos. Es el único elemento que debe preservarse para recuperar el estado del evento.

---

## Por qué offline-first

Un sistema que depende de internet entrega el control de la operación a un factor externo. Action EventOS toma la decisión opuesta:

- **Sin dependencia de señal.** Opera en carpas, sótanos, locaciones sin cobertura.
- **Latencia mínima.** Las respuestas se sirven desde el mismo equipo local, en milisegundos.
- **Privacidad de datos.** Los datos de asistentes no salen del perímetro del evento.
- **Resiliencia.** Un corte de internet no afecta la operación en absoluto.

---

## Instrucciones de compilación (desarrolladores)

Requisito: Go 1.22 o superior.

La SPA compilada se versiona en `web/dist`, de modo que `go build ./cmd/eventpos` funciona directamente desde un clon, sin Node.js. Para regenerar la interfaz tras cambios en el frontend, ejecuta `npm run build` dentro de `web/`.

```bash
bash scripts/build.sh
```

Los artefactos se generan en `dist/`. El script compila de forma cruzada binarios para Windows (`.exe`) y macOS, sin necesidad de CGO (driver SQLite en Go puro).

---

## Instrucciones de prueba

```bash
bash scripts/test.sh
```

Suite de pruebas en verde con el detector de condiciones de carrera (`-race`), en 29 paquetes — incluyendo concurrencia, recuperación ante fallos, seguridad por rol e integración HTTP de extremo a extremo.

---

## Stack tecnológico

| Componente | Tecnología | Razón |
|---|---|---|
| Servidor | Go (binario único) | Rendimiento, portabilidad, sin runtime externo |
| Base de datos | SQLite (modo WAL) vía `modernc.org/sqlite` | Embebida en el proceso, sin servidor de BD separado; driver en Go puro, **sin CGO** |
| UI | SPA embebida en el binario con `go:embed` | Sin dependencias de archivos externos |
| Realtime | Server-Sent Events (SSE) | Tableros de Pendientes sin polling |
| Autenticación | argon2id + cookie de sesión | Operación rápida en tableta o celular |
| Concurrencia | Single-writer serializado | Garantía de integridad sin locks manuales |

---

*Powered by CROODY*
