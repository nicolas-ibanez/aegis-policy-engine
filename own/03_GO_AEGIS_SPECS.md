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

## 2. State Management & Concurrency (Demo Configuration)

### 2.1. In-Memory Datastore
* **Estructura Base**: Instanciar un mapa global `map[string]DriverState` para simular la tabla de DynamoDB.
* **Estado Inicial Requerido**: Precargar un chofer con datos al límite para forzar el fallo del LLM en el demo.
  * Ej: `DRV-Juan` con `HoursDrivenToday: 3`.

### 2.2. Concurrency Safety (Mutex)
* **Requisito Estricto**: Todo acceso al mapa en memoria debe estar protegido por **`sync.RWMutex`** para evitar condiciones de carrera (Race Conditions).
* **Operaciones de Lectura**: Utilizar **`RLock()`** y **`RUnlock()`** al consultar el estado actual del chofer.
* **Operaciones de Escritura**: Utilizar **`Lock()`** y **`Unlock()`** al persistir una transacción válida (después de la validación matemática).
* **Bloqueo de Transacción**: Usar **`defer`** inmediatamente después de adquirir el bloqueo para garantizar la liberación del recurso.

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

### 4.1. Deterministic Validation Logic
* **Extracción**: Decodificar el `JSON` proveniente de Python (`ExecutionIntent`).
* **Consulta de Estado**: Buscar `driver_id` en el mapa protegido por Mutex.
* **Evaluación Matemática**: 
  * Sumar `HoursDrivenToday` (histórico) + `EstimatedHours` (solicitado).
  * **Regla de Negocio (Ley 18.290)**: Si la suma **> 5**, la validación falla.

### 4.2. Routing & HTTP Status Mapping
* **Ruta de Éxito**:
  * Adquirir `Lock()`.
  * Actualizar `HoursDrivenToday` en el mapa.
  * Liberar `Unlock()`.
  * Retornar **`HTTP 200 OK`**.
* **Ruta de Rechazo (Compliance Failure)**:
  * Cancelar transacción (no mutar el mapa).
  * Construir el objeto **`SemanticRejection`** especificando el límite superado.
  * Retornar **`HTTP 400 Bad Request`** con el JSON codificado para gatillar el **Retry Loop** en Python.