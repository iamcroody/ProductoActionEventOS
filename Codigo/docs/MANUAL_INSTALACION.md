# Manual de Instalación — Action EventOS

**Guía de instalación para operadores de evento y desarrolladores**

---

## Uso en Windows (operadores de evento)

### Requisitos

- Computador con Windows 10 o superior.
- Red Wi-Fi local activa (para que los demás dispositivos se conecten).
- Sin requisitos adicionales: no se instalan dependencias, runtimes ni servicios.

### Pasos

1. **Copiar la carpeta del sistema** al computador del evento (USB, carpeta de red o descarga directa). La carpeta contiene el binario `ActionEventOS.exe` y los archivos de soporte.

2. **Ejecutar `ActionEventOS.exe`** con doble clic.

3. **Permitir el acceso en la red** si Windows Firewall muestra un aviso:
   - Selecciona "Redes privadas" (o la red del evento).
   - Toca **Permitir acceso**.
   - Este paso es necesario para que los celulares y tablets puedan conectarse. Sin él, el sistema solo es accesible desde el mismo equipo.

4. La terminal muestra el banner de inicio con **dos códigos QR** en caracteres ASCII y sus direcciones:
   - **STAFF (privado)** → `http://192.168.x.x:8080/?k=…` para cajeros, meseros y cocina. Lleva una llave secreta; no lo compartas con los clientes. La IP varía según la red.
   - **CLIENTES (público)** → `http://192.168.x.x:8090` para que los asistentes pidan desde su celular. Puedes proyectarlo o imprimirlo.

5. El **navegador se abre automáticamente** en el equipo con la consola de gestión (`http://localhost:8080`).

6. Ingresa con las credenciales de primer arranque:
   - Usuario: `action`
   - Contraseña: `croody`

   La pantalla de inicio de sesión no muestra estas credenciales. **Cámbialas inmediatamente** en Administración → Operadores.

### Estructura de la carpeta tras el primer arranque

```
ActionEventOS/
├── ActionEventOS.exe
├── data/                   ← creada automáticamente
│   ├── eventpos.db         ← base de datos
│   └── backups/            ← respaldos automáticos y manuales
└── ...
```

La carpeta `data/` se crea en el mismo directorio desde donde se ejecuta el binario. **Esta carpeta es el backup del evento.** Al finalizar, cópiala para preservar el historial completo.

### Apagar el servidor

Cierra la ventana de la terminal o presiona `Ctrl+C`. El sistema realiza un respaldo automático antes de apagarse.

---

## Uso en Linux / macOS (operadores técnicos)

El procedimiento es equivalente al de Windows con el binario correspondiente al sistema operativo:

- Linux: `ActionEventOS` (sin extensión)
- macOS: `ActionEventOS` (sin extensión)

Para ejecutar desde terminal:

```bash
chmod +x ./ActionEventOS
./ActionEventOS
```

En macOS, si el sistema bloquea el binario por ser de un desarrollador no identificado:

```
Sistema Preferencias → Seguridad y Privacidad → Abrir igualmente
```

O desde terminal: `xattr -c ./ActionEventOS`.

---

## Conexión de dispositivos en red local

Una vez que el servidor está corriendo:

1. Conecta todos los dispositivos (celulares, tablets, computadores de cajeros/meseros/cocina) a la **misma red Wi-Fi** que el equipo donde corre el servidor.
2. Escanea el **código QR** que corresponda:
   - El personal escanea el **QR de STAFF (privado)**, que transporta la llave de acceso a la gestión. Sin esa llave, la consola es invisible (responde 404) para el resto de la red.
   - Los asistentes escanean el **QR de CLIENTES (público)**, que abre únicamente la web del cliente.
3. El dispositivo abre el navegador con la URL de acceso.
4. El operador inicia sesión con su usuario y PIN.

Los mismos QR están disponibles dentro de la app en la pantalla **Accesos** (Administración → Acceso clientes). Si un QR no es legible, el administrador puede dictar la URL manualmente: gestión `http://192.168.x.x:8080` (requiere la llave) y clientes `http://192.168.x.x:8090`, donde la IP es la que aparece en la terminal del servidor.

> Los dispositivos en redes de datos móviles (4G/5G) **no** pueden acceder al sistema. Deben estar en la misma red Wi-Fi local.

---

## Opciones de línea de comandos

El binario acepta las siguientes opciones:

| Flag | Defecto | Descripción |
|---|---|---|
| `-port` | `8080` | Puerto de la consola de gestión (staff). |
| `-client-port` | `8090` | Puerto de la web de clientes (QR), aislado de gestión. |
| `-data` | `data` | Carpeta donde se guardan la base de datos y los respaldos. |
| `-open` | `true` | Abrir automáticamente el navegador al iniciar. |
| `-admin-key` | *(aleatoria)* | Llave de acceso a gestión. Si se omite, se genera una por arranque; también puede fijarse con la variable de entorno `AEOS_ADMIN_KEY`. |

Ejemplo con opciones personalizadas:

```bash
./ActionEventOS -port 9090 -client-port 9091 -data /ruta/a/datos -open=false
```

---

## Compilación para desarrolladores

### Requisitos

- Go 1.22 o superior: [https://go.dev/dl/](https://go.dev/dl/)
- Git (para clonar el repositorio).

### Compilar

```bash
# Compilar todos los targets
bash scripts/build.sh
```

Los artefactos se generan en `dist/`:

```
dist/
├── ActionEventOS.exe        # Windows (amd64)
├── ActionEventOS-macos      # macOS (arm64)
└── ActionEventOS-linux      # Linux (amd64)
```

El script compila el frontend (`web/dist`) y luego compila de forma cruzada los binarios. El driver SQLite es Go puro, por lo que no se requiere CGO ni toolchain de C.

### Ejecutar en modo desarrollo

```bash
go run ./cmd/eventpos
```

El servidor inicia en `http://localhost:8080` con los parámetros por defecto.

### Ejecutar pruebas

```bash
bash scripts/test.sh
```

Las pruebas incluyen el detector de carreras de Go (`-race`) para validar la concurrencia del single-writer.

---

## Recuperación desde respaldo

En caso de necesitar restaurar el estado del sistema desde un respaldo:

1. Detener el servidor (`Ctrl+C` o cerrar la terminal).
2. Localizar el archivo de respaldo en `data/backups/` (los respaldos son archivos `.db`).
3. Reemplazar el archivo `data/eventpos.db` con el archivo de respaldo.
4. Reiniciar el servidor con `ActionEventOS.exe`.

> Antes de reemplazar, verifica la integridad del respaldo desde la UI (Respaldos → Verificar) o manualmente calculando su SHA-256 y comparándolo con el registrado en el sistema.

---

*Powered by CROODY*
