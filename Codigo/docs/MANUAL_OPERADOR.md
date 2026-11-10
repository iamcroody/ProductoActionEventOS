# Manual del Operador — Action EventOS

**Para cajeros, meseros y estaciones de cocina / bar**

> Este manual explica paso a paso cómo usar Action EventOS desde un celular, tablet o computador durante el evento. No se necesitan conocimientos técnicos.

---

## Antes de empezar

### Qué necesitas

- Tu dispositivo (celular, tablet o computador) conectado a la **misma red Wi-Fi** que el equipo donde corre el sistema.
- El **QR de STAFF (privado)** que aparece en la terminal del equipo principal (o en la pantalla *Accesos* de la app), o la URL que el administrador te indique. Este QR lleva una llave de acceso: sin ella, la consola es invisible para tu dispositivo.
- Tu nombre de usuario y PIN asignados por el administrador.

> Usa siempre el **QR de staff**, no el de clientes. El QR de clientes solo abre la web de pedidos para los asistentes.

### Cómo acceder

1. Abre la cámara de tu celular y apúntala al **QR de staff** en pantalla.
2. Toca el enlace que aparece. La llave de acceso queda registrada en tu dispositivo y se abre la pantalla de inicio de sesión.
3. Escribe tu **nombre de usuario** y tu **PIN** de 4 dígitos.
4. Toca **Iniciar sesión**.

Qué verás: la pantalla principal del sistema según tu rol (cajero, mesero, cocina o bar).

---

## Rol: Cajero (Caja)

### 1. Abrir la caja

Al comenzar tu turno, lo primero es abrir la caja.

1. Ve a la sección **Caja** en el menú lateral.
2. Toca **Abrir caja**.
3. Ingresa el **monto inicial en efectivo** que tienes en tu caja (puede ser $0 si no manejas efectivo al inicio).
4. Toca **Confirmar apertura**.

Qué verás: un resumen de tu sesión de caja activa con el monto inicial registrado.

> Si ya hay una caja abierta por ti, el sistema te lleva directamente al resumen.

---

### 2. Encontrar a un asistente

Tienes dos formas de identificar al asistente que quiere hacer un consumo:

**Opción A — Búsqueda por nombre o documento**

1. En la sección de **Ventas** o **Nueva venta**, toca el campo de búsqueda.
2. Escribe el nombre del asistente o su número de documento.
3. El sistema muestra resultados en tiempo real. Toca el nombre correcto.

**Opción B — Escanear el QR del asistente**

1. Toca el ícono de cámara o **Escanear QR**.
2. Apunta la cámara al QR del asistente (puede estar en su pulsera, tarjeta o teléfono).
3. El sistema identifica al asistente automáticamente.

**Opción C — Registrar y vender en el momento**

Si el asistente no está registrado, puedes crearlo sin salir de la venta. Ingresa su **Documento** y su **Celular**, y continúa con la venta normalmente.

Qué verás: el nombre del asistente, su saldo disponible y su estado (activo o bloqueado).

> Si el asistente aparece **bloqueado**, no puede realizar consumos. Consulta al administrador.

---

### 3. Crear una venta

1. Con el asistente seleccionado, toca **Nueva venta**.
2. Selecciona los productos del menú (por categoría). Toca el producto para agregarlo; toca el número de cantidad para ajustarla.
3. Revisa el resumen: productos, cantidades y total.
4. Si hay **mesa o etiqueta**, ingresa el número de mesa en el campo correspondiente (ej.: "Mesa 4").
5. Toca **Cobrar**.

**Pantalla de pago:**

- Si el saldo del asistente cubre el total, el sistema lo cobra automáticamente del saldo y muestra **Venta confirmada**.
- Si el saldo no alcanza, el sistema muestra el saldo disponible y el monto faltante. Indica si el asistente paga la diferencia en **efectivo**:
  - Activa la opción **Confirmar pago en efectivo**.
  - Toca **Confirmar venta**.

Qué verás al terminar: el ticket de la venta con el detalle de lo cobrado desde saldo y lo cobrado en efectivo.

---

### 4. Venta con sobrante de efectivo (overage)

Cuando un asistente paga con efectivo más del total:

El sistema registra el cobro al precio exacto del consumo. El **cambio al asistente** es responsabilidad del cajero y se calcula manualmente. El diferencial de la caja aparece en el cierre.

---

### 5. Pago dividido entre varios asistentes

1. Al crear la venta, agrega los productos normalmente.
2. En la pantalla de pago, toca **Agregar pagador** y busca o escanea a cada asistente que paga.
3. El sistema divide el total según la política configurada (partes iguales o en cascada).
4. Confirma para cada asistente.

---

### 6. Enviar pedido a cocina o bar

Al crear una venta con productos que van a cocina o bar, el sistema los envía automáticamente al tablero correspondiente en el momento de confirmar la venta. No tienes que hacer nada adicional.

> Los productos con preparación en **cocina** van al tablero de cocina. Los de **bar** al tablero de bar. Los que no tienen estación asignada se marcan como entregados de inmediato.

---

### 7. Cerrar la caja

Al finalizar tu turno:

1. Ve a **Caja** y toca **Cerrar caja**.
2. Ingresa el **monto de efectivo contado** en tu caja al momento del cierre.
3. El sistema calcula:
   - **Efectivo esperado**: monto inicial + total cobrado en efectivo durante la sesión.
   - **Diferencial (variance)**: diferencia entre el efectivo esperado y el contado.
4. Toca **Confirmar cierre**.

Qué verás: el resumen de tu sesión con el diferencial. Si hay una diferencia, el administrador puede revisarla en reportes.

---

## Rol: Mesero

### Crear un pedido desde la mesa

1. Accede al sistema desde tu dispositivo.
2. Selecciona **Nuevo pedido**.
3. Escanea el QR del asistente o búscalo por nombre.
4. Elige los productos y la mesa (ej.: "Mesa 7").
5. Toca **Enviar pedido**.

El pedido llega al tablero de cocina o bar en segundos. Puedes seguir el estado desde tu dispositivo.

> El mesero **no** maneja pagos. El pago lo procesa siempre un cajero o el administrador.

---

## Rol: Cocina / Bar — Pendientes (entregas)

### El tablero de Pendientes

Cada ítem vendido es una **entrega para despachar desde bodega**: no se preparan comidas, se entregan productos. El tablero de **Pendientes** se actualiza automáticamente cuando llegan nuevos pedidos. No necesitas refrescar la página.

> Los pedidos que llegan **desde el celular de un cliente** (auto-pedido por QR) aparecen marcados **en amarillo** y rotulados como "Cliente".

**Cada ítem muestra tres cantidades:**

| Columna | Qué significa |
|---|---|
| **Pedido** | Unidades solicitadas en la venta |
| **Entregado** | Unidades ya despachadas al asistente |
| **Faltante** | Unidades que aún quedan por entregar |

### Entrega parcial

Si alguien pide varias unidades (por ejemplo, 6 aguas), puedes entregarlas **de a poco**:

1. Llega un nuevo pedido al tablero.
2. Toca **Entregar** para despachar **una unidad**; el contador de Entregado sube y el de Faltante baja.
3. Toca **Entregar todo** para despachar de una vez las unidades restantes del ítem.
4. Cuando Faltante llega a cero, el ítem queda completado.

Cada entrega queda registrada (qué se entregó, qué falta, quién lo despachó) y se refleja en todos los dispositivos conectados en tiempo real.

---

## Consultar el saldo de un asistente

Cualquier operador puede consultar el saldo de un asistente:

1. Ve a **Asistentes** o usa la búsqueda rápida.
2. Escribe el nombre o documento del asistente.
3. Toca su nombre para ver el detalle: saldo disponible, historial de recargas y consumos.

---

## Solución de problemas frecuentes

### El celular no carga la página

- Verifica que el celular esté conectado a la **misma red Wi-Fi** que el equipo donde corre el sistema. Una red de datos móviles no funciona.
- Pide al administrador que confirme la URL de acceso o vuelve a escanear el QR.
- Si el QR ya no es visible, el administrador puede indicar la dirección manualmente (ejemplo: `http://192.168.1.10:8080`).

### La sesión se cerró sola

- Las sesiones expiran por inactividad. Vuelve a ingresar con tu usuario y PIN.

### El producto no aparece en el menú

- El producto puede estar inactivo o sin stock. Consulta al administrador.

### No puedo confirmar la venta

- Verifica que el asistente no esté bloqueado.
- Si el saldo no alcanza y el asistente paga en efectivo, activa la opción de **Confirmar pago en efectivo** antes de confirmar.
- Verifica que haya una caja abierta (solo aplica para el rol cajero).

### El tablero de cocina no se actualiza

- Recarga la página una vez. Si el problema persiste, el administrador puede verificar el estado del servidor.
- Verifica la conexión Wi-Fi del dispositivo.

### No puedo iniciar sesión

- Verifica que el usuario y PIN sean correctos (distingue mayúsculas en el nombre de usuario).
- Consulta al administrador para restablecer el PIN.

---

*Powered by CROODY*
