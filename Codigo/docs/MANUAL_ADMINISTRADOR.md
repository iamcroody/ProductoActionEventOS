# Manual del Administrador — Action EventOS

**Para administradores y coordinadores del evento**

> Este manual cubre la configuración del sistema, la gestión del evento y el seguimiento operacional desde el rol `admin`.

---

## Acceso como administrador

1. Abre el navegador en el equipo donde corre el sistema: `http://localhost:8080`.
2. Ingresa con el usuario `action` y la contraseña `croody`.
3. El panel de administración es visible solo para los roles `admin` y `auditor`.

**Credenciales de primer arranque:** usuario `action`, contraseña `croody`. Por seguridad, la pantalla de inicio de sesión **no** muestra estas credenciales. Cámbialas antes de iniciar la operación desde **Administración → Operadores**.

---

## Modelo de acceso del evento

Action EventOS aísla por completo la gestión interna del acceso de los clientes, mediante **dos servidores en puertos distintos**:

- **Gestión (staff) — puerto `8080`.** Consola y API de operación. Es una superficie **hermética**: cualquier dispositivo de la red que no presente la llave de acceso recibe un 404 (la consola es invisible). El equipo anfitrión siempre entra. El personal accede desde su celular escaneando el **QR de staff privado**, que transporta la llave.
- **Clientes (QR) — puerto `8090`.** Solo la web del cliente. Los asistentes escanean el **QR de clientes público** para pedir desde su celular; desde ahí no pueden alcanzar la gestión.

Ambos QR se imprimen en la terminal al arrancar y están disponibles en la app en **Acceso clientes** (pantalla *Accesos*). La llave de gestión se genera por arranque; puedes fijarla con el flag `-admin-key` o la variable `AEOS_ADMIN_KEY`.

> Comparte el QR de staff únicamente con tu personal. El QR de clientes sí puede proyectarse o imprimirse para los asistentes.

---

## Red en vivo (monitor de red)

En **Red en vivo** (solo `admin`) verás el tráfico de la red local en tiempo real: los dispositivos conectados, un feed de peticiones (staff y clientes) y contadores de actividad. Es útil para confirmar que los celulares del personal y de los asistentes están entrando correctamente y para vigilar la operación durante el evento.

---

## Roles y permisos

Action EventOS tiene seis roles. El rol `admin` incluye todas las capacidades de todos los roles.

| Rol | Capacidades |
|---|---|
| `admin` | Acceso total. Configuración, eventos, usuarios, reportes, auditoría, respaldos. |
| `caja` | Abrir/cerrar caja, crear ventas, registrar asistentes, recargar saldo, ajustar inventario. |
| `mesero` | Crear pedidos (sin pago). Ver estado de pedidos. |
| `cocina` | Ver y actualizar tablero de cocina. |
| `bar` | Ver y actualizar tablero de bar. |
| `auditor` | Ver reportes, exportaciones y log de auditoría. Sin capacidad de modificar datos. |

> Un operador solo puede tener un rol. Para conceder acceso a múltiples funciones, asigna el rol de mayor privilegio que corresponda.

---

## Gestión de operadores

### Crear un operador

1. Ve a **Usuarios** en el menú de administración.
2. Toca **Nuevo usuario**.
3. Completa los campos:
   - **Nombre de usuario**: identificador de inicio de sesión (sin espacios).
   - **Nombre completo**: nombre visible en reportes.
   - **Rol**: selecciona el rol correspondiente.
   - **PIN**: 4 dígitos para acceso rápido desde tablet o celular.
   - **Contraseña** (opcional): para el usuario `admin` se recomienda una contraseña adicional.
4. Toca **Guardar**.

### Editar o desactivar un operador

En la lista de usuarios, toca el operador y modifica sus datos. Para suspender el acceso sin eliminar el historial, desactiva el campo **Activo**. El operador no podrá iniciar sesión pero sus registros de auditoría se conservan.

### Restablecer el PIN

En la ficha del operador, toca **Cambiar PIN** e ingresa el nuevo PIN.

---

## Gestión de eventos

### Ciclo de vida de un evento

Un evento pasa por tres estados:

```
draft → active → closed
```

- **draft**: En preparación. Se puede configurar sin que los operadores lo usen.
- **active**: En operación. Solo puede haber un evento activo al mismo tiempo.
- **closed**: Finalizado. Se genera un respaldo automático al cerrar. No se pueden registrar más ventas.

### Crear un evento

1. Ve a **Eventos** y toca **Nuevo evento**.
2. Completa los campos:
   - **Nombre del evento**: nombre descriptivo (ej.: "Gala ABC 2026 — Sede Norte").
   - **Fecha de inicio / fin** (opcionales): para referencia; no bloquean la operación.
   - **Saldo por defecto**: monto de recarga estándar al registrar asistentes (ej.: $50.000 COP).
   - **Política de pago dividido**:
     - `cascade`: se cobra desde el saldo del primer pagador; si no alcanza, continúa con el siguiente.
     - `strict_equal`: divide el total en partes exactamente iguales entre los pagadores.
3. Toca **Guardar**.

### Activar un evento

En la lista de eventos, toca el evento en estado `draft` y selecciona **Activar**. Solo puede haber un evento activo al mismo tiempo; si hay otro activo, primero ciérralo.

### Cerrar un evento

1. Ve a **Eventos** y toca el evento activo.
2. Toca **Cerrar evento**.
3. El sistema genera un **respaldo automático** de la base de datos.
4. El evento pasa a estado `closed`. Los reportes y el historial quedan disponibles para consulta.

---

## Configuración del menú

### Categorías

Las categorías organizan el menú visualmente.

1. Ve a **Categorías** y toca **Nueva categoría**.
2. Ingresa el nombre (ej.: "Bebidas", "Comidas", "Postres") y el orden de aparición.
3. Toca **Guardar**.

Para reordenar, edita el campo `Orden` de cada categoría. Para ocultar una categoría sin eliminarla, desactiva el campo **Activa**.

### Productos

1. Ve a **Productos** y toca **Nuevo producto**.
2. Completa los campos:

| Campo | Descripción |
|---|---|
| **Categoría** | Categoría a la que pertenece el producto. |
| **Nombre** | Nombre visible en el menú y en los tickets. |
| **Precio** | Precio en pesos COP (entero, sin centavos). |
| **Estación de preparación** | `cocina`, `bar` o `ninguna`. Determina a qué tablero llega el pedido. |
| **Controlar stock** | Si está activo, el sistema descuenta inventario con cada venta y puede bloquear la venta si no hay stock. |
| **Stock inicial** | Cantidad disponible al configurar el producto. |
| **Orden** | Posición en la lista del menú. |

3. Toca **Guardar**.

Para editar un producto (precio, stock, nombre), selecciónalo en la lista y modifica los campos. Para retirarlo del menú sin perder el historial, desactívalo (eliminación lógica).

---

## Gestión de inventario

### Carga inicial de inventario

El inventario se puede ingresar al crear los productos (campo **Stock inicial**) o ajustarse manualmente después.

Para ajustar el inventario de un producto:

1. Ve a **Inventario** y busca el producto.
2. Toca **Ajustar stock**.
3. Ingresa el **delta** (cantidad a sumar o restar) y el motivo:
   - `restock`: reposición de mercancía.
   - `waste`: merma o desperdicio.
   - `adjustment`: corrección de inventario.
4. Toca **Confirmar**.

Cada movimiento queda registrado en el historial de movimientos del producto, con el operador que lo realizó y la hora.

### Alerta de stock bajo

El dashboard muestra automáticamente los productos con stock por debajo del umbral (por defecto: 5 unidades). También puedes consultarlo en **Inventario → Stock bajo**.

---

## Gestión de asistentes

### Registrar un asistente individual

1. Ve a **Asistentes** y toca **Nuevo asistente**.
2. Ingresa los datos:
   - **Nombre visible** (requerido).
   - **Documento de identidad** (cédula).
   - **Correo electrónico**.
   - **Celular**.
   - **Saldo redimible o sin redimir**: elige si el asistente recibe saldo prepago que puede gastar (por defecto) o si **no** maneja saldo redimible y paga siempre en efectivo.
   - **Saldo inicial** (opcional; si no se ingresa, el sistema usa el valor por defecto del evento).
3. Toca **Guardar**.

> El documento, el correo y el celular son los datos con los que el asistente **valida su identidad** al pedir desde su celular por QR. Si no coinciden con el registro, el sistema lo rechaza.

El sistema genera un **código QR único** para el asistente (descargable como imagen PNG). Este QR puede imprimirse en una pulsera o tarjeta para que el cajero lo escanee, o para que el propio asistente pida desde su teléfono.

### Importación masiva de asistentes

Para cargar una lista grande de asistentes (ej.: importación desde el sistema de registro del evento):

1. Ve a **Asistentes → Importar**.
2. Prepara un archivo JSON con el formato:
   ```json
   {
     "attendees": [
       {"display_name": "Juan Pérez", "doc_id": "123456789", "initial_topup": 50000},
       {"display_name": "María García", "doc_id": "987654321"}
     ]
   }
   ```
3. Envía la importación. El sistema crea todos los asistentes en una sola operación.

### Recargar el saldo de un asistente

1. Busca al asistente en la lista o escanea su QR.
2. Toca **Recargar saldo**.
3. Ingresa el monto a acreditar.
4. Confirma.

El movimiento queda registrado en el ledger inmutable del asistente.

### Bloquear / desbloquear un asistente

Un asistente bloqueado no puede realizar consumos. Útil para reportes de pérdida de pulsera o problemas de seguridad.

1. Busca al asistente.
2. Toca **Bloquear** o **Desbloquear**.

---

## Dashboard y reportes

### Dashboard en tiempo real

Ve a **Reportes → Dashboard** para ver en tiempo real:

| Indicador | Descripción |
|---|---|
| Total vendido | Suma de todas las ventas del evento |
| Número de órdenes | Total de transacciones |
| Consumo promedio | Total vendido / número de asistentes con consumo |
| Asistentes activos | Con al menos un consumo registrado |
| Efectivo por overage | Efectivo recibido por encima del saldo disponible |
| Saldo consumido | Total debitado de saldos de asistentes |
| Productos top | Los más vendidos |
| Ventas por hora | Distribución temporal de ventas |
| Ventas por operador | Volumen por cajero/mesero |
| Stock bajo | Productos próximos a agotarse |

### Exportación de reportes

Ve a **Reportes → Exportar** y elige el reporte y el formato:

| Reporte | Contenido |
|---|---|
| `sales` | Detalle de todas las ventas |
| `products` | Resumen por producto (cantidad, ingresos) |
| `attendees` | Lista de asistentes con saldo final |
| `ledger` | Todos los movimientos del ledger de saldo |

Formatos disponibles: **CSV** (para Excel) y **JSON** (para integración técnica).

### Reporte de cierre

El **Reporte de cierre** es el consolidado de negocio que el director usa para cerrar el evento. Reúne en un solo informe los ingresos, el costo, la ganancia y el margen, los asistentes registrados y activos, los tiempos de atención, el desempeño por operador, los productos más vendidos y el estado del inventario.

Se descarga en dos formatos:

- **XLSX** — libro de Excel con varias hojas, listo para presentar.
- **CSV** — versión por secciones para análisis o integración.

### Propinas

En **Propinas**, el administrador ingresa la bolsa total y el sistema la reparte de forma transparente: los **tres meseros con mayor venta** y el **personal de aseo** reciben un **5% adicional**, y el resto se divide en partes iguales. La suma siempre cuadra exactamente con la bolsa ingresada.

---

## Respaldos e integridad

### Crear un respaldo manual

1. Ve a **Respaldos** y toca **Crear respaldo**.
2. El sistema genera una copia de la base de datos con su hash SHA-256.

Los respaldos se guardan en la carpeta `data/backups/` del equipo.

### Respaldos automáticos

- Al **arrancar el servidor**: se crea un punto de restauración desde el primer instante.
- De forma **periódica** (aproximadamente cada 10 minutos) durante la operación, con retención de las copias más recientes.
- Al **cerrar un evento**: se genera un respaldo automático.
- Al **apagar el servidor**: se genera un respaldo de cierre.
- En checkpoints WAL internos del motor de base de datos.

### Verificar la integridad de un respaldo

1. Ve a **Respaldos**, selecciona un respaldo de la lista.
2. Toca **Verificar integridad**.
3. El sistema calcula el SHA-256 del archivo y lo compara con el registrado al momento de la creación. Si coinciden, el respaldo está íntegro.

### Restaurar desde respaldo

La restauración es un procedimiento técnico descrito en el Manual de Instalación. En términos conceptuales: detener el servidor, reemplazar el archivo `data/eventpos.db` con el respaldo y reiniciar.

---

## Auditoría e integridad del sistema

### Log de auditoría

El log de auditoría registra toda acción relevante: logins, ventas, recargas, ajustes de inventario, cambios de configuración. Cada registro es **inmutable** (el motor de base de datos rechaza cualquier modificación o eliminación).

Para consultar el log:

1. Ve a **Auditoría**.
2. Filtra por tipo de acción, operador o asistente.
3. Exporta en CSV si necesitas un informe para terceros.

### Verificación de la cadena de hash

El log de auditoría y el ledger de saldo están protegidos por una **cadena de hash encadenada** (similar a un blockchain): cada registro incluye el hash del registro anterior. Si alguien modifica un registro en la base de datos, la cadena se rompe y el sistema lo detecta.

Para verificar la integridad:

1. Ve a **Auditoría → Verificar integridad**.
2. El sistema recorre toda la cadena y confirma si está íntegra o indica el primer registro donde se detectó una alteración.

También puedes ejecutar la verificación integral del sistema desde **Configuración → Integridad**, que incluye:
- Verificación de la cadena de auditoría.
- Conciliación de saldos (ledger vs. saldo en tabla de asistentes).
- Conciliación de inventario (movimientos vs. stock en tabla de productos).

---

## Configuración general

Ve a **Configuración** para ajustar los parámetros del sistema:

| Parámetro | Descripción |
|---|---|
| `currency` | Moneda del sistema (por defecto COP) |
| `default_topup` | Saldo de recarga por defecto al registrar asistentes |
| `org_name` | Nombre de la organización que aparece en reportes |

---

*Powered by CROODY*
