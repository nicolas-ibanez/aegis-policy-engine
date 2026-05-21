---
title: "03_GO_AEGIS_SPECS"
description: "Technical specifications, concurrency rules, and standard library mandates for Go shield"
version: "1.1.0"
tags: ["go", "golang", "mutex", "net/http", "security", "timeouts"]
last_updated: "2026-05-20"
---

# 03_GO_AEGIS_SPECS: DETERMINISTIC SHIELD (GO LAYER)

## 1. Core Development Directives

* **Standard Library Exclusivity**: Prohibición absoluta de frameworks HTTP externos (ej: `Gin`, `Fiber`). Utilizar exclusivamente **`net/http`** y **`encoding/json`**.
* **Separation of Concerns**: La lógica de enrutamiento HTTP debe estar **estrictamente aislada** de la lógica de validación matemática.
* **Deterministic Execution**: Go actúa como la Unidad Lógico-Aritmética (**ALU**). Ninguna decisión probabilística debe programarse en esta capa.

## 2. State Persistence (Dual Mode)

### 2.1. Mode Selection (Environment Variable)
* **Variable de Control**: `DEMO_MODE` (leída al iniciar el servidor con `os.Getenv`).
* **`DEMO_MODE=true`** (In-Memory Store):
  * Utilizar un mapa en memoria `map[string]DriverState` protegido por un `sync.RWMutex`.
  * Al iniciar el servidor, precargar los 3 drivers del demo script:
    * `DRV-Juan`: `HoursDrivenToday: 3`, `License: "A5"`, `CurrentStatus: "Available"`.
    * `DRV-Pedro`: `HoursDrivenToday: 4`, `License: "B"`, `CurrentStatus: "Available"`.
    * `DRV-Diego`: `HoursDrivenToday: 1`, `License: "A5"`, `CurrentStatus: "Available"`.
* **`DEMO_MODE=false`** (AWS DynamoDB):
  * **Librería Obligatoria**: Utilizar `github.com/aws/aws-sdk-go-v2`. Prohibido usar la v1.
  * **Tabla**: Conectar a la tabla `AegisDrivers` en la región `us-east-1`.
  * **Timeouts Estrictos**: Toda llamada a DynamoDB (`GetItem`, `UpdateItem`) debe estar envuelta en un `context.WithTimeout` de máximo **200 milisegundos**. Si la base de datos se degrada, el Gateway debe fallar rápido.
## 3. Pre-Flight Shield Implementation (Inbound)

### 3.1. Payload Sanitization
* **Límite de Longitud**: Ejecutar validación **O(1)**. Si `len(prompt) > 500`, retornar inmediatamente `HTTP 400 Bad Request`.
* **Motor Regex**: Compilar la expresión regular globalmente al iniciar el servidor usando **`regexp.MustCompile`** (evitar recompilación por cada request).
* **Patrón de Bloqueo**: Buscar coincidencias de inyección (`(?i)(ignore all|system prompt|drop table)`).
* **Fallo Rápido**: Si la Regex hace match, interrumpir ejecución y retornar **`HTTP 403 Forbidden`**. Costo de tokens: $0.

### 3.2. Internal Proxy Routing (Go -> Python)
* **Custom HTTP Client**: **Prohibición absoluta** de usar `http.DefaultClient`. Instanciar un `http.Client` estricto con un `Timeout` máximo de **15 segundos**. Esto previene fugas de memoria por *Goroutines* colgadas si la API del LLM se retrasa.
* **Failure States (Manejo de Caídas)**: Si la petición interna falla, interceptar el error y proteger al cliente externo.
  * **Connection Refused (Python caído)**: Retornar al cliente externo **`HTTP 503 Service Unavailable`**.
  * **Timeout Exceeded (LLM/Python bloqueado)**: Retornar al cliente externo **`HTTP 504 Gateway Timeout`**.

## 4. Post-Flight Shield Implementation (Outbound)

### 4.1. Deterministic Matrix Validation Logic
* **Extracción**: Decodificar el `JSON` proveniente de Python (`ExecutionIntent`).
* **Consulta de Estado**: Obtener el estado del `driver_id` desde el store activo (in-memory o DynamoDB según `DEMO_MODE`).
* **Evaluación Matricial (Reglas Duras)**: 
  1. **Regla 1 — Cuantitativa (Ley 18.290)**: `HoursDrivenToday` + `EstimatedHours` **<= 5**.
  2. **Regla 2 — Cualitativa (Certificación)**: `License` **== "A5"** (requerida para despachos a "Mina Sur").
  3. **Regla 3 — Estado Operacional**: `CurrentStatus` **== "Available"**. Si el chofer está `"OnRoute"` u `"OffDuty"`, Go bloquea la transacción.
* Las reglas se evalúan en orden. Si **cualquiera** falla, la validación se rechaza inmediatamente. El `SemanticRejection` debe especificar **exactamente cuál de las tres reglas falló** (Cuantitativa, Cualitativa o Estado Operacional).

### 4.2. Routing & HTTP Status Mapping
* **Ruta de Éxito**:
  * Persistir la mutación: sumar las `EstimatedHours` al registro del driver en el store activo.
  * Retornar **`HTTP 200 OK`**.
* **Ruta de Rechazo (Compliance Failure)**:
  * Abortar transacción (No ejecutar mutación en el store).
  * Construir el objeto **`SemanticRejection`** especificando qué regla de la matriz falló (Cuantitativa, Cualitativa o Estado Operacional).
  * Retornar **`HTTP 400 Bad Request`** con el JSON codificado para gatillar el **Retry Loop** en Python.