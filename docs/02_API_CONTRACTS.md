---
title: "02_API_CONTRACTS"
description: "Strict API endpoints, JSON schemas, and data structures for Aegis"
version: "1.0.0"
tags: ["api", "json", "go-structs", "pydantic", "contracts"]
last_updated: "2026-05-20"
---

# 02_API_CONTRACTS: DATA STRUCTURES & ENDPOINTS
# 02_API_CONTRACTS: DATA STRUCTURES & ENDPOINTS

## 1. External Interface (Client -> Go Aegis)

### 1.1. Inbound Request

- **Endpoint**: `POST /api/v1/dispatch`
- **Actor**: External Client (UI/Terminal).
- **Responsibility**: Punto de entrada único. Sometido a **Escudo Pre-Vuelo** (Regex/Length).
- **Payload Esperado**:

```json
{
  "prompt": "Se cayó el turno, manda a Juan a la Mina Sur. El viaje dura 3 horas."
}
````

### 1.2. Outbound Response

* **Success (200 OK)**: Operación validada y persistida.
* **Client Error (403 Forbidden)**: Rechazo Pre-Vuelo (Prompt Injection).

```json
{
  "status": "blocked",
  "reason": "PRE_FLIGHT_FAILURE",
  "message": "Malicious payload or out-of-scope intent detected."
}
```

---

## 2. Internal Interface A (Go Aegis -> Python Agent)

### 2.1. Agent Trigger

* **Endpoint**: `POST http://localhost:5000/internal/v1/agent/process`
* **Actor**: Go Aegis.
* **Responsibility**: Iniciar el razonamiento LLM y el **Bucle de Autocorrección**.
* **Payload Esperado**:

```json
{
  "session_id": "req-8f72c",
  "clean_prompt": "Se cayó el turno, manda a Juan a la Mina Sur. El viaje dura 3 horas."
}
```

---

## 3. Internal Interface B (Python Agent -> Go Aegis)

### 3.1. Post-Flight Validation & Execution

* **Endpoint**: `POST http://localhost:8080/internal/v1/execute`
* **Actor**: Python Agent.
* **Responsibility**: Solicitar mutación de estado. Sometido a **Escudo Post-Vuelo** (Matemática Determinista).
* **Payload Esperado (Structured Intent)**:

```json
{
  "intent": "dispatch_driver",
  "driver_id": "DRV-Juan",
  "estimated_hours": 3
}
```

### 3.2. Deterministic Rejection Contract (El Motor del Retry Loop)

* **Status Code**: `HTTP 400 Bad Request`.
* **Regla de Negocio**: Este JSON debe ser procesado por el bloque `try/except` de Python e inyectado literalmente al contexto del LLM.
* **Payload de Rechazo**:

```json
{
  "status": "blocked",
  "reason": "POLICY_VIOLATION_MATRIX",
  "message": "Driver DRV-Pedro rejected. Reason: Requires License A5, holds License B.",
  "action_required": "Select another driver with hours_driven_today <= 2 AND License == A5."
}
```

---

## 4. Code-Level Schemas (IDE Context Binding)

### 4.1. Go Structs (`Aegis`)

* **Ubicación**: Usar en la capa de transporte HTTP y validación en Go.

```go
// Estructura de Ejecución Interna (Recibida de Python)
type ExecutionIntent struct {
    Intent         string `json:"intent"`
    DriverID       string `json:"driver_id"`
    EstimatedHours int    `json:"estimated_hours"`
}

// Estructura de Rechazo Semántico (Enviada a Python)
type SemanticRejection struct {
    Status         string `json:"status"`
    Reason         string `json:"reason"`
    Message        string `json:"message"`
    ActionRequired string `json:"action_required"`
}

// Estructura de Estado del Conductor (In-Memory / DynamoDB Item)
type DriverState struct {
    DriverID         string `json:"DriverID"`
    CurrentStatus    string `json:"CurrentStatus"`    // "Available", "OnRoute", "OffDuty"
    HoursDrivenToday int    `json:"HoursDrivenToday"`
    License          string `json:"License"`
}
```

### 4.2. Python Pydantic Models (`Agent`)

* **Ubicación**: Usar en `FastAPI` y validación de `Structured Outputs` del LLM.

```python
from pydantic import BaseModel, Field

# Esquema para forzar el output del LLM
class DispatchIntent(BaseModel):
    intent: str = Field(description="Must be 'dispatch_driver'")
    driver_id: str = Field(description="Exact ID of the selected driver")
    estimated_hours: int = Field(description="Estimated duration of the trip in hours")

# Esquema para parsear el error de Go
class AegisRejection(BaseModel):
    status: str
    reason: str
    message: str
    action_required: str

```

```
```
