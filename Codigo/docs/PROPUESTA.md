# Action EventOS — Propuesta Ejecutiva para ABC Corporation

**Consola de operación de eventos: offline, un ejecutable, sin filas, sin errores de saldo.**

> *"No desarrollamos software. Diseñamos tecnología que mejora la experiencia humana."*
> — CROODY

---

## El problema

Gestionar consumos en un evento corporativo con hojas de cálculo o sistemas en la nube genera tres cuellos de botella inevitables: **búsquedas lentas** que forman filas, **errores de saldo** cuando dos operadores actúan al mismo tiempo sobre el mismo asistente, y **dependencia total de internet** — si cae la señal en la locación, se detiene la operación.

---

## La solución

Action EventOS es una consola de operación de eventos que corre como un único ejecutable en el computador del evento — sin internet, sin instalación de bases de datos ni servidores externos. Al iniciarse, detecta la IP de la red local y genera **dos códigos QR**: uno **privado para el staff** y uno **público para los clientes**. Cajeros, meseros y cocina operan desde sus celulares con el QR de staff; los asistentes piden directamente desde su propio teléfono con el QR de clientes, sin hacer fila. Ambos accesos están **aislados en puertos separados**: el cliente nunca puede tocar la gestión interna, y la consola es invisible para cualquier dispositivo que no presente la llave del staff.

---

## Por qué importa

- **Venta en segundos.** El cajero busca al asistente por nombre o cédula, agrega productos y cobra. Si el saldo no alcanza, el excedente se registra en efectivo — sin pasos adicionales.
- **Auto-pedido sin mesero.** El asistente escanea su QR personal, valida identidad (nombre, correo, cédula, celular) y arma su pedido desde el celular. El saldo se descuenta solo; el pedido llega marcado en amarillo a Pendientes.
- **Cero errores de saldo.** La arquitectura garantiza que dos ventas simultáneas sobre el mismo asistente nunca cobren dos veces ni dejen saldo negativo — es imposible por diseño, no por política.
- **Entrega sin caos.** Si alguien pide 6 aguas, se entregan de a poco. El sistema registra pedido, entregado y faltante, a nombre de quién y en qué ubicación.
- **Control de ganancias en vivo.** Cada producto tiene precio de venta y costo al por mayor; el dashboard muestra ingresos, costo, margen y ganancia por producto en tiempo real.
- **Propinas justas y transparentes.** El administrador ingresa la bolsa total; el sistema reparte: top-3 meseros y aseo reciben 5% adicional, el resto por partes iguales — suma exacta, sin discusiones.
- **Reporte de cierre listo para presentar.** Al terminar, el director descarga un consolidado del evento —ingresos, costo, ganancia, asistentes, operadores e inventario— en **Excel (XLSX) y CSV**.
- **Visibilidad de la red en vivo.** Un monitor de *Red en vivo* muestra al administrador los dispositivos conectados y el tráfico de staff y clientes en tiempo real.
- **Sin internet ni instalación.** Opera en carpas, sótanos o locaciones sin señal. Los datos de los asistentes no salen del perímetro del evento.

---

## Lo que nos diferencia

| Diferenciador | Qué significa en la práctica |
|---|---|
| **Auto-pedido del cliente por QR** | El asistente pide desde su celular sin mesero. Valida identidad contra el registro corporativo; si no coincide, no entra. Su sesión está aislada: jamás toca la gestión interna. |
| **Candado de evento** | Al iniciar el evento, menú, precios e inventario quedan inamovibles. Nadie puede cambiar las reglas con la operación andando. Registrar asistentes y recargar saldo sí siguen disponibles. |
| **Integridad a prueba de fraude** | El ledger de saldo y el log de auditoría son inmutables con cadena de hash SHA-256. Si alguien modifica un registro directamente en la base de datos, el sistema lo detecta. Auditoría irrebatible. |
| **Saldo redimible vs sin-redimible** | Se configura por asistente: quién puede gastar su saldo prepago y quién solo paga en efectivo. |
| **Un solo ejecutable, cero instalación** | Copia la carpeta, doble clic en `ActionEventOS.exe`. Sin instalar Node.js, Python, base de datos ni servidor web. Windows, macOS y Linux. |

---

## Cómo se ve en 3 minutos

Ver **[DEMO.md](DEMO.md)** para el guion completo con pantallas y frases para el presentador. El recorrido:

1. Ejecutar `ActionEventOS.exe` → el QR aparece en la terminal. Sin instalar nada.
2. Configurar menú con precios y costos al por mayor (la ganancia por producto se calcula en vivo).
3. Registrar asistentes con nombre, correo, cédula y celular; asignar saldo prepago (redimible o sin-redimible).
4. Iniciar el evento → queda en candado; configuración bloqueada.
5. Un asistente escanea su QR con el celular, valida identidad y pide. El pedido llega en amarillo a Pendientes.
6. La caja procesa ventas en segundos; entrega parcial queda registrada.
7. El dashboard muestra ganancias en vivo. El cierre genera respaldo automático.

---

## Solidez técnica

Action EventOS tiene una **suite de pruebas automatizadas en verde** con detector de condiciones de carrera (`-race`), distribuida en 29 paquetes — incluyendo concurrencia, recuperación ante fallos, seguridad por rol e integración HTTP de extremo a extremo. El ledger de saldo es append-only con hash encadenado; cualquier modificación directa a la base de datos es detectable. Los respaldos automáticos (al arrancar, periódicos, al cerrar el evento y al apagar) y la recuperación WAL garantizan consistencia ante un corte de corriente sin pérdida de datos.

---

## Siguiente paso

Agendemos una demostración en vivo de 15 minutos — sin presentación, directo al sistema funcionando.
Solo necesitamos un equipo Windows y una red Wi-Fi.

**Contacto:** CROODY · _Powered by CROODY_
