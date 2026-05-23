"""
agent.py: Probabilistic LLM Orchestration Layer.
Enforces deterministic tool use and strict schema validation via Pydantic.
Maintains state-blindness to offload architectural constraints to the Go gateway.
"""
import os
import json
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, Field
import anthropic

app = FastAPI()
client = anthropic.Anthropic(api_key=os.environ.get("ANTHROPIC_API_KEY"))

class AgentRequest(BaseModel):
    session_id: str
    clean_prompt: str

class DispatchIntent(BaseModel):
    intent: str = Field(description="Must be 'dispatch_driver'")
    driver_id: str = Field(description="Exact ID of the selected driver")
    estimated_hours: int = Field(description="Estimated duration of the trip in hours")
    destination: str = Field(description="Exact destination of the dispatch")

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
    Does not validate business rules (State Blindness pattern).
    """
    try:
        response = client.messages.create(
            model="claude-sonnet-4-20250514",
            max_tokens=1024,
            system=SYSTEM_PROMPT,
            messages=[
                {"role": "user", "content": req.clean_prompt}
            ],
            tools=[TOOL_SCHEMA],
            tool_choice={"type": "tool", "name": "dispatch_driver"}
        )
        
        for block in response.content:
            if block.type == "tool_use" and block.name == "dispatch_driver":
                print(f"[AGENT - {req.session_id}] Generated Intent: {json.dumps(block.input)}")
                return block.input
                
        raise HTTPException(status_code=500, detail="LLM did not return the requested tool.")
        
    except Exception as e:
        print(f"[AGENT - {req.session_id}] Error: {str(e)}")
        raise HTTPException(status_code=500, detail=str(e))
