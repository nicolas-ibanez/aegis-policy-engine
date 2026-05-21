---
title: "04_PYTHON_AGENT_SPECS"
description: "LLM orchestration, retry loop logic, and information asymmetry configurations for Python agent"
version: "1.1.0"
tags: ["python", "fastapi", "llm", "retry-loop", "pydantic", "anthropic"]
last_updated: "2026-05-20"
---

# 04_PYTHON_AGENT_SPECS: PROBABILISTIC BRAIN (PYTHON LAYER)

## 1. Core Development Directives

* **Framework Exclusivity**: Utilizar **`FastAPI`** para la API interna y **`pydantic`** para la validación de esquemas.
* **Network Isolation**: Prohibición absoluta de conexiones hacia bases de datos. Interacción restringida a APIs de LLM externas y *endpoints* internos de Go.
* **Role Definition**: El agente opera exclusivamente como **Traductor Semántico** (Lenguaje Natural -> JSON Estructurado) y **Orquestador de Fallos**.

## 2. LLM Configuration & Information Asymmetry

### 2.1. Model Specification (Anthropic)
* **Modelo Obligatorio**: Utilizar exclusivamente la API de Anthropic con el modelo **`claude-sonnet-4-20250514`**.
* **Justificación Técnica**: Mayor estabilidad y determinismo semántico al generar *Structured Outputs* (JSON estricto) frente a alternativas, reduciendo alucinaciones de formato que romperían el contrato de datos con Go.
* **Integración de SDK**: Usar el paquete oficial `anthropic` para Python, implementando la funcionalidad nativa de **Tool Use** (`tool_choice`) para forzar la estructura.

### 2.2. System Prompt Mandates
* **Reglas de Negocio Estáticas**: El *prompt* **debe** contener las normativas operativas base (Ej: límite legal de 5 horas continuas).
* **Ceguera de Estado (Blindness)**: El *prompt* **no debe** contener el historial transaccional en tiempo real (Ej: horas conducidas actualmente por cada chofer).
* **Catálogo Base**: Proveer únicamente la lista de entidades disponibles (IDs de choferes válidos) sin su métrica de estado.

### 2.3. Structured Outputs Enforcement
* **Desactivación de Texto Libre**: El agente tiene **prohibido** emitir respuestas en texto plano al usuario. Toda salida debe ser obligada a pasar por la herramienta de ejecución.
* **Esquema Pydantic**: Inyectar el modelo **`DispatchIntent`** (definido en `02_API_CONTRACTS`) como el *schema* de los parámetros de la herramienta (Tool) enviada a Claude.

## 3. The Orchestration & Retry Loop (Critical Path)

### 3.1. Bucle de Autocorrección (Circuit Breaker)
* **Límite Estricto**: Definir constante **`MAX_RETRIES = 3`**.
* **Condición de Inicio**: Envolver la llamada a Claude y la petición HTTP a Go dentro de un bloque **`while`** o **`for`** estrictamente limitado por `MAX_RETRIES`.

### 3.2. Manejo de Rechazos (Try/Except Logic)
* **Captura de Excepción**: Interceptar el código **`HTTP 400 Bad Request`** devuelto por el *Escudo Post-Vuelo* (Go).
* **Des-serialización**: Parsear el cuerpo de la respuesta utilizando el modelo Pydantic **`AegisRejection`**.

### 3.3. Inyección de Contexto y Re-Evaluación
* **Mutación del Historial**: Añadir la respuesta original de Claude (Tool Use) y el mensaje de error decodificado de Go a la lista de mensajes (historial de conversación).
* **Rol del Mensaje de Error**: Inyectar el rechazo bajo el rol **`user`** procesando el resultado de la herramienta (`tool_result`), con un formato imperativo.
  * Ej: `"Ejecución fallida. Motor determinista devolvió: {rejection.message}. Requerimiento: {rejection.action_required}."`
* **Reintento**: Ejecutar nueva llamada a la API de Claude pasando el historial enriquecido con la falla.

### 3.4. Terminal Failure Escalation
* **Agotamiento de Intentos**: Si el bucle alcanza la iteración `MAX_RETRIES + 1`, romper la ejecución inmediatamente.
* **Prevención de Gasto (FinOps)**: No realizar ninguna llamada de compensación adicional a la API de Anthropic.
* **Respuesta de Fuga**: Retornar **`HTTP 422 Unprocessable Entity`** al cliente original (vía Go), indicando: `"Restricción Insatisfactible. Intervención humana requerida."`