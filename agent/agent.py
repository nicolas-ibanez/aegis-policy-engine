"""
agent.py: Probabilistic LLM Orchestration Layer.
Enforces deterministic tool use and strict schema validation via Pydantic.
Maintains state-blindness to offload architectural constraints to the Go gateway.
"""
import os
import json
import httpx
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, Field
from typing import Literal
import anthropic

app = FastAPI()
client = anthropic.Anthropic(api_key=os.environ.get("ANTHROPIC_API_KEY"))

class AgentRequest(BaseModel):
    session_id: str
    clean_prompt: str

class DispatchIntent(BaseModel):
    intent: Literal["dispatch_driver"] = Field(description="Must be exactly 'dispatch_driver'")
    driver_id: str = Field(description="Exact ID of the selected driver")
    estimated_hours: int = Field(description="Estimated duration of the trip in hours")
    destination: str = Field(description="Exact destination of the dispatch")

class AegisRejection(BaseModel):
    status: str
    reason: str
    message: str
    action_required: str | None = None

MAX_RETRIES = 3

SYSTEM_PROMPT = """You are a fleet dispatch semantic translator.
Convert natural language requests into structured dispatch intents.

Operating Rules:
1. Legal limit: 5 hours maximum continuous driving.
2. Route requirement: Dispatches to "Mina Sur" strictly require License A5.
3. Valid Driver IDs catalog: DRV-Juan, DRV-Pedro, DRV-Diego."""

TOOL_SCHEMA = {
    "name": "dispatch_driver",
    "description": "Formulate a deterministic dispatch intent.",
    "input_schema": DispatchIntent.model_json_schema()
}

@app.post("/internal/v1/agent/process")
async def process_prompt(req: AgentRequest):
    """
    Ingests pre-flighted prompts and translates them into structured JSON intents via Anthropic Tool Use.
    Implements a 3-retry auto-correction loop relying on the Go deterministic shield's semantic rejections.
    Maintains state-blindness pattern.
    """
    try:
        messages = [{"role": "user", "content": req.clean_prompt}]
        
        async with httpx.AsyncClient() as http_client:
            for attempt in range(MAX_RETRIES + 1):
                response = client.messages.create(
                    model="claude-sonnet-4-20250514",
                    max_tokens=1024,
                    system=SYSTEM_PROMPT,
                    messages=messages,
                    tools=[TOOL_SCHEMA],
                    tool_choice={"type": "tool", "name": "dispatch_driver"}
                )
                
                messages.append({"role": "assistant", "content": response.content})
                
                tool_use = None
                for block in response.content:
                    if block.type == "tool_use" and block.name == "dispatch_driver":
                        tool_use = block
                        break
                
                if not tool_use:
                    raise HTTPException(status_code=500, detail="LLM did not return the requested tool.")
                
                intent_json = tool_use.input
                print(f"[AGENT - {req.session_id}] Attempt {attempt + 1}: Generated Intent: {json.dumps(intent_json)}")
                
                go_resp = await http_client.post(
                    "http://localhost:8080/internal/v1/execute",
                    json=intent_json
                )
                
                if go_resp.status_code == 200:
                    print(f"[AGENT - {req.session_id}] Success on attempt {attempt + 1}")
                    return intent_json
                elif go_resp.status_code == 400:
                    rejection = AegisRejection(**go_resp.json())
                    error_msg = f"Ejecución fallida. Motor determinista devolvió: {rejection.message}. Requerimiento: {rejection.action_required}"
                    print(f"[AGENT - {req.session_id}] Shield Rejected: {error_msg}")
                    
                    messages.append({
                        "role": "user",
                        "content": [
                            {
                                "type": "tool_result",
                                "tool_use_id": tool_use.id,
                                "content": error_msg,
                                "is_error": True
                            }
                        ]
                    })
                else:
                    raise HTTPException(status_code=go_resp.status_code, detail=go_resp.text)
            
            print(f"[AGENT - {req.session_id}] MAX_RETRIES exhausted.")
            raise HTTPException(
                status_code=422,
                detail="Restricción Insatisfactible. Intervención humana requerida."
            )
            
    except HTTPException:
        raise
    except Exception as e:
        print(f"[AGENT - {req.session_id}] Error: {str(e)}")
        raise HTTPException(status_code=500, detail=str(e))
