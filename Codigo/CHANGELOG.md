# Changelog — Action EventOS

## [0.2.0] — 2026-06 — Pendientes, cliente-QR, candado de evento

### Operación
- **Pendientes (entregas)** reemplaza el concepto de cocina/preparación: cada ítem vendido es una entrega a despachar desde bodega, con **entrega parcial** (p. ej. 6 aguas de a poco; se rastrea pedido/entregado/faltante). Tablero en vivo (SSE).
- **Auto-pedido del cliente por QR**: web negra independiente (`/cliente`), el asistente valida su identidad (nombre/correo/cédula/celular) contra el registro corporativo, ve su saldo y pide; el saldo se descuenta con las mismas garantías de integridad que en caja. Los pedidos de cliente llegan **en amarillo** a Pendientes.
- Seguridad: sesión de cliente por cookie HMAC aislada — los clientes no pueden acceder a rutas de gestión.

### Reglas de negocio
- **Candado de evento**: la configuración (menú, precios, inventario, asistentes, saldo) solo se permite en **borrador**; al **iniciar** el evento queda inamovible (409). Excepción: bloquear/desbloquear el bono de un asistente sigue permitido.
- **Saldo redimible vs sin-redimible**: al registrar se elige si el asistente recibe saldo (default) o **no** (paga en efectivo); email/cédula/celular para validar identidad.
- Renombres anti-bobos: "Caja" (sin POS), "Pendientes". Asistente de primeros pasos + modo lite.

### Calidad
- Suite de pruebas en verde con `-race` (29 paquetes), incluidas concurrencia, recuperación ante fallos, seguridad/permisos, e integración HTTP de extremo a extremo, con verificación end-to-end en navegador.


All notable changes to this prototype are documented here.
Format loosely follows [Keep a Changelog](https://keepachangelog.com/).

## [0.1.0] — 2026-06 — Prototipo inicial

### Plataforma (L0)
- Servidor local único (Go) con SQLite embebido (WAL) y UI web embebida en el binario.
- Modelo de concurrencia **single-writer** + transacciones `BEGIN IMMEDIATE`: integridad de saldo e inventario garantizada bajo operación concurrente.
- Detección automática de IP de red local + código QR de acceso al iniciar.
- Autenticación de operadores por PIN (argon2id) y sesiones por rol.
- Configuración, respaldos automáticos/manuales con verificación de integridad (sha256).
- Hub de eventos en tiempo real (Server-Sent Events) para cocina/bar.

### Módulos de dominio
- Usuarios, Eventos, Categorías, Productos, Inventario, Asistentes, Saldo, Caja,
  Ventas/Consumos (venta atómica + pago compartido), Cocina/Bar en tiempo real,
  Dashboard/Reportes/Exportaciones, Logs/Auditoría con cadena de hash.

### Calidad
- Pruebas de concurrencia con el detector de carreras (`-race`).
- Auditoría inmutable encadenada por hash (a prueba de manipulación).
