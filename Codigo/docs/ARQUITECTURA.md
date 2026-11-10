# Arquitectura Técnica — Action EventOS

**Para revisores técnicos, arquitectos de software y evaluadores de ABC Corporation**

> Este documento describe las decisiones de diseño, el modelo de datos y las garantías de integridad de Action EventOS v0.2.0.

---

## Filosofía de diseño

Action EventOS parte de una premisa de diseño deliberada: **la operación no puede depender de infraestructura externa**. Un evento masivo en una locación con conectividad inestable no puede permitirse que la red pague la operación de consumos. La respuesta es un sistema *offline-first* o *local-first*: toda la lógica, los datos y la interfaz corren en el equipo del evento. Internet es irrelevante.

La segunda premisa es la **integridad sin concesiones**: en un sistema de consumos donde cientos de asistentes y docenas de operadores interactúan simultáneamente, cualquier condición de carrera es un error de negocio (doble descuento, saldo negativo, stock negativo). La arquitectura resuelve esto structuralmente, no con locks manuales.

---

## Diagrama de arquitectura

```
┌─────────────────────────────────────────────────────────────────┐
│                    Red Wi-Fi local del evento                   │
│                                                                 │
│  [Celular cajero]  [Celular mesero]  [Tablet cocina/bar]        │
│        │                 │                   │                  │
│        └─────────────────┴───────────────────┘                  │
│                          │  HTTP/SSE                            │
│                          ▼                                      │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │              ActionEventOS  (binario único Go)            │  │
│  │                                                           │  │
│  │  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌─────────┐  │  │
│  │  │ HTTP API │  │  SPA UI  │  │ SSE Hub  │  │ Backups │  │  │
│  │  │  (L4–L7) │  │(embebida)│  │(realtime)│  │ (L1)    │  │  │
│  │  └────┬─────┘  └──────────┘  └────┬─────┘  └────┬────┘  │  │
│  │       │                           │              │        │  │
│  │  ┌────▼───────────────────────────▼──────────────▼────┐  │  │
│  │  │                   App Core (L3)                     │  │  │
│  │  │  audit hash-chain · ledger append · integridad      │  │  │
│  │  └────────────────────────┬────────────────────────────┘  │  │
│  │                           │                               │  │
│  │  ┌────────────────────────▼────────────────────────────┐  │  │
│  │  │              Single Writer (L2)                      │  │  │
│  │  │       goroutine serializada · BEGIN IMMEDIATE        │  │  │
│  │  └────────────────────────┬────────────────────────────┘  │  │
│  │                           │                               │  │
│  │  ┌────────────────────────▼────────────────────────────┐  │  │
│  │  │           SQLite WAL embebido (L0)                   │  │  │
│  │  │         eventpos.db  (archivo local)                 │  │  │
│  │  └─────────────────────────────────────────────────────┘  │  │
│  └───────────────────────────────────────────────────────────┘  │
│                                                                 │
│  [Equipo del evento — Windows / Linux / macOS]                  │
└─────────────────────────────────────────────────────────────────┘
```

---

## Capas del sistema (L0 – L7)

| Capa | Nombre | Responsabilidad |
|---|---|---|
| L0 | Almacenamiento | SQLite en modo WAL; toda la persistencia. |
| L1 | Infraestructura | Configuración, respaldos, utilidades de red (detección de IP LAN). |
| L2 | Escritura serializada | `writer.Writer`: goroutine única de escritura, transacciones `BEGIN IMMEDIATE`. |
| L3 | Core de aplicación | `app.App`: contexto compartido, `WriteAudit`, `AppendLedger`, `VerifyAuditChain`. |
| L4 | Auth | Sesiones por cookie, argon2id para PIN y contraseña. |
| L5 | Módulos de dominio | Eventos, Usuarios, Asistentes, Caja, Ventas, Inventario, Pendientes (entregas), Cliente (auto-pedido por QR), Reportes y cierre, Monitor de red, Auditoría. |
| L6 | Servidor HTTP | Dos servidores aislados (gestión y clientes), candado de acceso por llave, middlewares de autenticación y rol, montaje de módulos. |
| L7 | UI embebida | SPA empaquetada en el binario con `embed.FS`; servida como archivos estáticos. |

---

## El modelo de escritura serializada (Single-Writer)

Este es el núcleo de las garantías de integridad del sistema.

### El problema que resuelve

En un servidor HTTP convencional, múltiples goroutines de Go atienden peticiones concurrentes. Si dos cajeros procesan simultáneamente una venta sobre el mismo asistente, ambas goroutines pueden leer el mismo saldo antes de que cualquiera lo actualice, resultando en un doble descuento. Prevenir esto requiere habitualmente mecanismos de locking explícitos o un sistema de base de datos con serializable isolation.

### La solución

Action EventOS usa un **único canal de escritura serializado** (`writer.Writer`): todas las operaciones de escritura (INSERT, UPDATE, DELETE) se envían a una goroutine dedicada que las ejecuta **una a una**, en orden, dentro de transacciones `BEGIN IMMEDIATE`. SQLite no necesita locking adicional porque nunca hay dos escritores simultáneos.

El resultado:

- **Sin doble descuento.** La segunda venta ve el saldo ya debitado por la primera.
- **Sin stock negativo.** La segunda venta de un producto con stock 1 ve stock 0 y falla correctamente.
- **Sin condiciones de carrera.** Probado con el detector de carreras de Go (`-race`) bajo carga concurrente.

Las lecturas no pasan por el writer y usan una conexión de solo lectura separada, por lo que la latencia de lectura no se ve afectada.

---

## La venta atómica

La operación de venta es una **transacción SQL atómica** que incluye:

1. Verificar que el evento esté activo.
2. Verificar que el asistente esté activo y tenga saldo suficiente (o que se haya confirmado el pago en efectivo).
3. Verificar y decrementar el stock de cada producto vendido (si `track_stock = 1`).
4. Registrar la orden y sus ítems.
5. Registrar el pago (`payments`, INMUTABLE).
6. Registrar el movimiento de saldo en `balance_ledger` (INMUTABLE, encadenado por hash).
7. Actualizar el saldo en caché del asistente.
8. Registrar los movimientos de stock en `stock_movements` (INMUTABLE).
9. Escribir el registro de auditoría en `audit_log` (INMUTABLE, encadenado por hash).

Si cualquier paso falla, la transacción entera se revierte. No hay estados intermedios.

---

## Ledger inmutable y cadena de hash (tamper evidence)

### Ledger de saldo

La tabla `balance_ledger` es la **fuente de verdad de todos los movimientos de dinero**. Es append-only: los triggers de la base de datos rechazan cualquier UPDATE o DELETE con un error de ABORT.

Cada fila incluye:
- `amount`: monto firmado (positivo para crédito, negativo para débito).
- `balance_after`: saldo resultante tras el movimiento.
- `ref_payment_id`: referencia UNIQUE al pago que originó el movimiento. Esto previene estructuralmente que un mismo pago se registre dos veces en el ledger.
- `prev_ledger_id` y `chain_hash`: enlace al registro anterior y hash encadenado.

### Cadena de hash del ledger

Cada registro de ledger contiene `chain_hash = SHA-256(hash_anterior || payload)`. Si alguien modifica directamente un registro en la base de datos (incluso con acceso de archivo), el hash de ese registro no coincidirá con el esperado por el siguiente registro, y la verificación lo detectará.

### Log de auditoría

`audit_log` sigue el mismo patrón: append-only, encadenado por hash. Registra toda acción del sistema: logins, ventas, recargas, ajustes, cambios de configuración. La cadena cubre toda la vida del sistema, no por asistente.

### Verificación

`GET /api/audit/verify` recorre la cadena completa y devuelve `{intact: true}` o `{intact: false, broken_at: "<id>"}`.

`GET /api/integrity` verifica adicionalmente:
- Que el saldo en caché de cada asistente coincida con la suma de su ledger.
- Que el stock en caché de cada producto coincida con la suma de sus movimientos.

---

## Realtime: Server-Sent Events (SSE)

El tablero de **Pendientes** y el monitor **Red en vivo** usan **Server-Sent Events** en lugar de polling o WebSockets. El cliente abre una conexión HTTP persistente a `/api/stream?topic=pendientes|monitor|*` y el servidor envía eventos cuando llegan nuevas órdenes, cambia el estado de una entrega o se registra tráfico en la red.

El `realtime.Hub` es un bus de eventos en memoria con un buffer de 1024 mensajes. Los módulos de dominio publican eventos en el hub al completar una operación; el hub los distribuye a todos los clientes suscritos al topic correspondiente.

Topics y tipos de eventos SSE:
- `pendientes` · `ticket`: nueva orden con ítems para despachar.
- `pendientes` · `ticket_update`: cambio en la entrega de un ítem (entregado parcial / total).
- `monitor` · `req`: una petición observada en la red local (alimenta *Red en vivo*).

---

## Dos servidores aislados y acceso por QR

Action EventOS expone **dos servidores HTTP independientes** en puertos distintos, de modo que la superficie de gestión nunca queda accesible desde el canal público de clientes:

| Servidor | Puerto (flag) | Sirve |
|---|---|---|
| **Gestión (staff)** | `8080` (`-port`) | Consola SPA + API de operación. Incluye `/api/monitor/*` (solo `admin`). |
| **Clientes (QR)** | `8090` (`-client-port`) | Únicamente la web del cliente y `/api/cliente/*`. Cualquier `/api/*` desconocido responde 404 JSON; no hace *fallback* al SPA, para no filtrar rutas de gestión. |

Al iniciar, el servidor:

1. Detecta la IP de la interfaz de red local activa (no loopback) mediante `netutil.LANIP()`.
2. Construye la URL pública de clientes (`http://<LAN-IP>:8090`) y la URL privada de staff (`http://<LAN-IP>:8080/?k=<llave>`).
3. Imprime en la terminal **dos códigos QR ASCII**: **STAFF (privado)** y **CLIENTES (público)**. Los mismos QR están disponibles dentro de la app en la pantalla *Accesos*.
4. Abre automáticamente el navegador del equipo en `http://localhost:8080`.

### Candado de acceso a gestión (consola hermética)

El puerto de gestión está protegido por un *gate* que lo hace **invisible** para cualquier dispositivo de la red que no presente la llave de acceso:

- El equipo anfitrión (loopback) siempre pasa.
- Un dispositivo de la LAN presenta la llave una sola vez mediante `?k=<llave>` (transportada por el QR de staff privado). El *gate* fija entonces una cookie `aeos_gate` `HttpOnly` con `SameSite=Strict` y redirige para quitar la llave de la barra de direcciones (comparación en tiempo constante).
- Cualquier otra petición —incluido `/login` o cualquier ruta de API— recibe un **404** plano, indistinguible de "aquí no hay nada". Un cliente curioso que adivine el puerto no encuentra página de inicio de sesión ni superficie que atacar.

La llave se genera fresca por arranque, salvo que se fije con `-admin-key` o la variable de entorno `AEOS_ADMIN_KEY`. Sobre ambos servidores se aplican además cabeceras de endurecimiento (`X-Content-Type-Options`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, `Cross-Origin-Opener-Policy: same-origin`).

---

## Modelo de datos — resumen

```
events (1) ──< attendees (N)            # asistentes pertenecen a un evento
events (1) ──< categories (N)
events (1) ──< products (N)
events (1) ──< cash_sessions (N)
events (1) ──< orders (N)

attendees (1) ──< balance_ledger (N)    # INMUTABLE, fuente de verdad de saldo
attendees (1) ──< payments (N)          # INMUTABLE
attendees (1) ──< order_splits (N)

orders (1) ──< order_items (N)
orders (1) ──< payments (N)             # INMUTABLE
orders (1) ──< order_splits (N)

products (1) ──< stock_movements (N)    # INMUTABLE, fuente de verdad de stock

audit_log                               # INMUTABLE, global, hash-encadenado
```

### Tablas inmutables (append-only)

| Tabla | Razón |
|---|---|
| `balance_ledger` | Fuente de verdad de saldo; toda modificación es un fraude. |
| `payments` | Registro de pago; no se puede alterar. |
| `stock_movements` | Historial de inventario; base para auditoría de mermas. |
| `audit_log` | Registro de acciones del sistema; debe ser irrebatible. |

La inmutabilidad se enforcea con triggers SQL a nivel de motor, no de aplicación.

---

## Respaldos

| Tipo | Cuándo se genera |
|---|---|
| `manual` | Por el administrador desde la UI o API. |
| `auto` | Al arrancar el servidor, de forma periódica (~10 min, con retención de las copias más recientes), al cerrar un evento y al apagar el servidor. |
| `wal_checkpoint` | En checkpoints internos del motor WAL. |

El respaldo de arranque garantiza un punto de restauración desde el primer instante; el respaldo periódico acota la pérdida ante un corte de corriente a unos pocos minutos. Cada respaldo registra su hash SHA-256 en `backups`. La API `/api/backups/verify` calcula el hash del archivo en disco y lo compara con el registrado, detectando cualquier corrupción o modificación del archivo.

---

## Decisiones tecnológicas

### ¿Por qué Go?

- **Binario único sin runtime externo.** El operador del evento no necesita instalar Node.js, Python, Java o ningún entorno. Copia la carpeta, ejecuta el binario.
- **Rendimiento nativo.** Maneja cientos de conexiones concurrentes con footprint de memoria mínimo.
- **Modelo de concurrencia claro.** El patrón goroutine + channel es idiomático en Go y facilita el single-writer.

### ¿Por qué SQLite?

- **Embebida en el proceso.** No hay un servidor de base de datos separado que levantar, monitorear o mantener.
- **Driver en Go puro (`modernc.org/sqlite`), sin CGO.** Permite compilación cruzada trivial a Windows y macOS desde una sola máquina, sin toolchain de C.
- **WAL mode.** Permite lecturas concurrentes mientras una escritura está en progreso, sin bloquear la UI.
- **Durabilidad garantizada.** Cada commit en WAL mode es durable (fsync). Un corte de corriente no corrompe la base de datos.
- **Archivo único.** El respaldo completo de la base de datos es copiar un archivo.

### ¿Por qué no la nube?

La decisión de no usar una solución cloud no es una limitación técnica: es una postura de diseño. Los datos de los asistentes de un evento corporativo son sensibles. La operación en campo no puede depender de un SLA externo. El costo de operar un servidor cloud para cada evento es injustificado cuando el problema se resuelve mejor localmente. La nube se puede agregar como capa de sincronización post-evento en versiones futuras, sin cambiar la arquitectura core.

---

*Powered by CROODY*
