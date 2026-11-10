# Guion de Demo — Action EventOS

**Demostración ejecutiva (~3 minutos)** · Prototipo para ABC Corporation · _Powered by CROODY_

> **Para el presentador.** Este guion recorre un evento completo: configurar en borrador → iniciar (candado) → operar (caja, mesero y **cliente por QR**) → entregar → cerrar. Cada escena indica qué mostrar y qué decir.
>
> **Preparación:** servidor iniciado; navegador en `http://localhost:8080`; un **segundo dispositivo (celular)** en la misma Wi-Fi para escanear los QR. La terminal muestra **dos QR**: uno de **staff (privado)** y uno de **clientes (público)**. Credenciales de gestión: `action` / `croody`.

---

## Escena 1 — El problema (~15 s, sin pantalla)
> "En un partido con cientos de asistentes, hoy todo es una hoja de Excel: filas, búsquedas lentas, errores de saldo, y si cae el internet, se detiene la operación. Action EventOS resuelve eso — y funciona 100% local."

## Escena 2 — Arranque local + dos QR (~15 s)
**Pantalla:** la terminal al ejecutar `ActionEventOS.exe`.
> "Un solo ejecutable. Levanta el servidor, detecta la IP de la red y genera **dos códigos QR**: uno **privado para el staff** y uno **público para los clientes**. Sin instalar nada, sin internet."
- Señalar los dos QR. El de **staff** (`http://192.168.x.x:8080`) lleva una llave: para cualquier otro dispositivo la consola "no existe". El de **clientes** (`http://192.168.x.x:8090`) es el que se proyecta o imprime para los asistentes.

## Escena 3 — Ingreso anti-bobos (~10 s)
**Pantalla:** login. Escribir usuario `action`, contraseña `croody`.
> "Acceso por rol. El sistema recuerda lo que falta antes de empezar."
- Al entrar sin evento, aparece el **asistente de primeros pasos** (crear evento → menú → asistentes → iniciar).

## Escena 4 — Configurar en BORRADOR (~30 s)
**Pantalla:** Administración.
1. **Eventos** → "Nuevo evento" (recarga por defecto $50.000). Queda en **borrador**.
2. **Menú** → crear productos con **precio y costo (valor al por mayor)**. Mostrar que calcula la **ganancia por unidad** en vivo (ej. Six pack $22.000, costo $15.400 → gana $6.600).
3. **Asistentes** → registrar con **nombre, correo, cédula, celular** (datos que validan al cliente por QR), y elegir si recibe **saldo redimible** o es **sin dinero redimible**.
> "Todo se prepara con calma en borrador: menú, precios, inventario, asistentes."

## Escena 5 — INICIAR el evento = CANDADO (~20 s)
**Pantalla:** Eventos → "Iniciar evento".
> "Al iniciar, **todo queda inamovible**: precios, inventario y el saldo de cada asistente. Nadie puede alterar las reglas con el evento andando."
- **Demostrar el candado:** ir a Menú → intentar editar un precio → aparece **"Evento iniciado — configuración bloqueada"**.
- Aclarar la excepción: "Lo único que sí se puede, por seguridad, es **bloquear el bono** de un asistente."

## Escena 6 — Venta en Caja (~20 s)
**Pantalla:** Caja.
- Buscar asistente (nombre/cédula) **o escanear su QR**. Agregar productos. Cobrar.
> "Una venta en segundos: descuenta del saldo, y si se pasa, cobra el excedente en efectivo. Sin filas."

## Escena 7 — ⭐ Cliente por QR (el diferenciador) (~30 s)
**Pantalla:** el **celular** escanea el QR del asistente → abre la web negra del cliente.
1. El cliente ingresa **nombre, correo, cédula, celular**. _(Si no coinciden con el registro, lo rechaza.)_
2. Ve su **saldo** y el menú; arma su pedido. El carrito **no deja exceder el saldo**.
3. Pide → su saldo se descuenta solo.
> "El asistente se autogestiona desde su teléfono. Abarcamos más sin más personal — y es seguro: el cliente jamás toca la gestión."

## Escena 8 — Pendientes con entrega parcial (~20 s)
**Pantalla:** gestión → **Pendientes**.
> "El pedido del cliente llega **en amarillo**, marcado como 'Cliente'."
- Mostrar columnas **Pedido / Entregado / Faltante**. Tocar **"Entregar"** una vez en las aguas.
> "Si alguien pide 6 aguas, se entregan **de a poco** y queda registrado qué se entregó y qué falta. No se preparan comidas: se **despacha** desde bodega."

## Escena 9 — Ganancias y propinas (~20 s)
**Pantalla:** Dashboard y Propinas.
- **Dashboard:** Ingresos, **Costo**, **Ganancia** y **Margen**, y ganancia por producto.
- **Propinas:** ingresar la bolsa → reparte; **top-3 meseros + aseo reciben 5% más**, el resto por partes iguales (suma exacta).
> "El director ve en tiempo real cuánto se ganó, y el reparto de propinas es justo y transparente."

## Escena 10 — Trazabilidad, cierre y reporte (~20 s)
**Pantalla:** Auditoría → "Verificar integridad"; luego Caja → cerrar; Evento → cerrar; y el **Reporte de cierre**.
> "Cada acción queda registrada con cadena a prueba de manipulación. Al cerrar, cuadra la caja y **respalda solo**. Y el director se lleva el **Reporte de cierre** —ingresos, costo, ganancia, asistentes y productos— descargable en **Excel y CSV**."

## Cierre (~10 s)
> "Offline, en un ejecutable, anti-bobos, seguro y con números en vivo. No entregamos software: diseñamos tecnología que mejora la experiencia humana. — CROODY."

---

## Notas para preguntas
- **¿Sin internet?** Todo es local; el celular entra por la red Wi-Fi del evento, no por internet.
- **¿Y si se va la luz?** La base usa WAL: al reabrir, queda consistente; hay respaldos automáticos.
- **¿Doble cobro / fraude?** Imposible por diseño: un solo escritor serializa todo, el saldo y el inventario tienen guardas, y el ledger es inmutable con hash encadenado.
- **¿El cliente puede ver datos de otros o de gestión?** No: su sesión está aislada en un puerto separado; solo accede a su saldo y su menú, nunca a la consola.
- **¿Y si alguien adivina la dirección de la consola?** La gestión vive en otro puerto y exige una llave (la del QR de staff). Sin ella responde 404: es **invisible**, no hay siquiera una pantalla de login que atacar.
- **¿Cómo sé qué pasa en la red?** En **Red en vivo** (solo admin) ves en tiempo real los dispositivos conectados y cada petición de staff y clientes.
- **¿Personalización?** Menú, precios, redimible, roles y modo lite (ocultar inventario/dashboard) son configurables por evento.
