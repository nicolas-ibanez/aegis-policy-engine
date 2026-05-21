---
title: "01_SYSTEM_ARCHITECTURE"
description: "Core architecture map for Aegis Deterministic AI Gateway"
version: "1.0.0"
tags: ["architecture", "go", "python", "microservices", "llm"]
last_updated: "2026-05-20"
---

# 01_SYSTEM_ARCHITECTURE: AEGIS API GATEWAY

## Core Philosophy
* **Design Pattern**: **API Gateway** con **Microservicios Desacoplados** (Patrón Sandwich).
* **Segregación de Responsabilidades**: Separación estricta entre **ejecución determinista** (Go) y **razonamiento probabilístico** (Python/LLM).
* **Asimetría de Información**: El Agente LLM opera con **ceguera de estado**; no posee acceso al estado actual de la base de datos para forzar la validación determinista.

## Component Boundaries and Network Rules
* **Cliente Externa**: Interactúa **únicamente** con la capa Go (puerto `:8080`). No tiene acceso directo a Python ni a la base de datos.
* **Capa Aegis (Go)**: Expuesta públicamente en `:8080`. Es el **único** componente autorizado para mutar el estado y comunicarse con el cliente. Se comunica internamente con Python.
* **Capa Agente (Python)**: Expuesta internamente en `:5000`. **Aislada de internet público**. Se comunica con APIs LLM externas (`OpenAI`/`Anthropic`) y devuelve *payloads* a Go. **Prohibición absoluta** de conexión a la base de datos.

## Data Flow Pipeline (The Sandwich Pattern)

### Fase 1: Inbound Pre-Flight (Go Aegis)
* **Objetivo**: **SecOps** y **FinOps** preventivo.
* **Entrada**: Texto crudo (`string`) proveniente del Cliente.
* **Ejecución**: 
  * Validación **O(1)** de límite de longitud (prevención de saturación de contexto).
  * Escaneo **Regex** contra inyecciones de *prompt* (`system prompt`, `ignore all instructions`).
* **Salida**: 
  * Rechazo: `HTTP 403 Forbidden` (interrupción de circuito, **0 tokens consumidos**).
  * Aprobación: Reenvío de *payload* limpio al microservicio Python (`HTTP POST`).

### Fase 2: Probabilistic Processing (Python Agent)
* **Objetivo**: Traducción de lenguaje natural a **Intención Estructurada**.
* **Entrada**: Texto limpio reenviado por Go.
* **Ejecución**:
  * Inyección del texto en un **System Prompt** estricto.
  * Llamada a API de LLM comercial.
  * Forzado de formato de salida vía **Structured Outputs** (`JSON`).
* **Salida**: Objeto `JSON` estricto representando la intención de negocio. Reenviado a Go vía ruta interna `/internal/v1/execute`.

### Fase 3: Outbound Post-Flight (Go Aegis)
* **Objetivo**: **Compliance determinista** y protección de estado.
* **Entrada**: Objeto `JSON` generado por el LLM.
* **Ejecución**:
  * Extracción de variables clave (ej: `driver_id`, `estimated_hours`).
  * Consulta de estado actual en la **Capa de Persistencia**.
  * Aplicación de **álgebra booleana y matemática de restricciones** (Lógica de negocio / Cumplimiento legal).
* **Salida**:
  * Aprobación: Mutación de estado, devuelve `HTTP 200 OK`.
  * Rechazo: Aborta transacción, devuelve `HTTP 400 Bad Request` con esquema de error semántico (gatilla **Bucle de Autocorrección** en Python).

### Fase 4: State Persistence (AWS DynamoDB)
* **Implementación**: Base de datos NoSQL *Serverless* (AWS DynamoDB).
* **Esquema Single-Table**: Tabla `AegisDrivers`. Partition Key: `DriverID` (String).
* **Control de Latencia**: Go debe ejecutar consultas con un *Timeout* estricto. Si DynamoDB no responde, se aborta la transacción para no colgar el *Gateway*.
* **Escalabilidad**: Al delegar el estado a AWS, las instancias de Go pueden escalar horizontalmente (stateless) sin preocuparse por condiciones de carrera locales.

## Strict AI IDE Mandates
* **Regla 1**: El código Go **no procesa lenguaje natural**. Usa `net/http` estándar.
* **Regla 2**: El código Python **no implementa lógica de base de datos** ni reglas matemáticas complejas. 
* **Regla 3**: El bucle de reintento (`Retry Loop`) reside **exclusivamente** en el microservicio de Python, con un límite duro de interrupciones (`MAX_RETRIES = 3`).