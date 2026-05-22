package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// Compiled once at startup per spec §3.1 — recompiling per-request would
// add O(n) overhead and defeat the purpose of the O(1) pre-flight shield.
var injectionPattern = regexp.MustCompile(`(?i)(ignore all|system prompt|drop table)`)

type ExecutionIntent struct {
	Intent         string `json:"intent"`
	DriverID       string `json:"driver_id"`
	EstimatedHours int    `json:"estimated_hours"`
	Destination    string `json:"destination"`
}

type SemanticRejection struct {
	Status         string `json:"status"`
	Reason         string `json:"reason"`
	Message        string `json:"message"`
	ActionRequired string `json:"action_required,omitempty"`
}

type DispatchRequest struct {
	Prompt string `json:"prompt"`
}

type DriverState struct {
	DriverID         string `json:"DriverID"`
	CurrentStatus    string `json:"CurrentStatus"`    // "Available" | "OnRoute" | "OffDuty"
	HoursDrivenToday int    `json:"HoursDrivenToday"`
	License          string `json:"License"`
}

type inMemoryStore struct {
	mu      sync.RWMutex
	drivers map[string]DriverState
}

type server struct {
	demoMode  bool
	memStore  *inMemoryStore   // nil when demoMode == false
	dynamoDB  *dynamodb.Client // nil when demoMode == true
	// http.DefaultClient carries no timeout; a stalled LLM upstream would leak goroutines
	// indefinitely. 15 s covers the Python agent's full MAX_RETRIES=3 budget.
	httpClient *http.Client
}

func main() {
	demoMode := os.Getenv("DEMO_MODE") == "true"

	srv := &server{
		demoMode:   demoMode,
		httpClient: &http.Client{Timeout: 15_000_000_000},
	}

	if demoMode {
		srv.memStore = newInMemoryStore()
		log.Println("[AEGIS] DEMO_MODE=true — in-memory store ready")
	} else {
		client, err := newDynamoDBClient()
		if err != nil {
			log.Fatalf("[AEGIS] DynamoDB init failed: %v", err)
		}
		srv.dynamoDB = client
		log.Println("[AEGIS] DEMO_MODE=false — DynamoDB client ready (us-east-1, AegisDrivers)")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/dispatch", srv.handleDispatch)

	addr := ":8080"
	log.Printf("[AEGIS] Listening on %s (demoMode=%v)", addr, demoMode)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("[AEGIS] Server error: %v", err)
	}
}

func (s *server) handleDispatch(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	var req DispatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, SemanticRejection{
			Status:  "blocked",
			Reason:  "INVALID_PAYLOAD",
			Message: "Request body must be valid JSON with a 'prompt' field.",
		})
		return
	}

	// O(1) length check — evaluated before regex to avoid any string scanning cost
	// on oversized payloads (context saturation / FinOps protection).
	if len(req.Prompt) > 500 {
		log.Printf("[AEGIS - PRE-FLIGHT] BLOCKED reason=LENGTH_EXCEEDED len=%d latency=%s",
			len(req.Prompt), time.Since(start))
		writeJSON(w, http.StatusBadRequest, SemanticRejection{
			Status:  "blocked",
			Reason:  "PRE_FLIGHT_FAILURE",
			Message: "Prompt exceeds maximum allowed length of 500 characters.",
		})
		return
	}

	// Regex shield: zero tokens consumed on match — circuit breaker before LLM call.
	if injectionPattern.MatchString(req.Prompt) {
		log.Printf("[AEGIS - PRE-FLIGHT] BLOCKED reason=INJECTION_DETECTED latency=%s",
			time.Since(start))
		writeJSON(w, http.StatusForbidden, SemanticRejection{
			Status:  "blocked",
			Reason:  "PRE_FLIGHT_FAILURE",
			Message: "Malicious payload or out-of-scope intent detected.",
		})
		return
	}

	log.Printf("[AEGIS - PRE-FLIGHT] PASSED prompt_len=%d latency=%s",
		len(req.Prompt), time.Since(start))

	// Placeholder: proxy to Python agent (Phase 2) not yet implemented.
	w.WriteHeader(http.StatusOK)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func newInMemoryStore() *inMemoryStore {
	store := &inMemoryStore{drivers: make(map[string]DriverState)}

	store.drivers["DRV-Juan"] = DriverState{
		DriverID: "DRV-Juan", HoursDrivenToday: 3, License: "A5", CurrentStatus: "Available",
	}
	store.drivers["DRV-Pedro"] = DriverState{
		DriverID: "DRV-Pedro", HoursDrivenToday: 4, License: "B", CurrentStatus: "Available",
	}
	store.drivers["DRV-Diego"] = DriverState{
		DriverID: "DRV-Diego", HoursDrivenToday: 1, License: "A5", CurrentStatus: "Available",
	}

	return store
}

func newDynamoDBClient() (*dynamodb.Client, error) {
	// aws-sdk-go-v2 only; v1 is prohibited per spec §2.1.
	cfg, err := config.LoadDefaultConfig(
		context.Background(),
		config.WithRegion("us-east-1"),
	)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	return dynamodb.NewFromConfig(cfg), nil
}
