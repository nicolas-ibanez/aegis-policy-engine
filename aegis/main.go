package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
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

type AgentRequest struct {
	SessionID   string `json:"session_id"`
	CleanPrompt string `json:"clean_prompt"`
}

type DriverState struct {
	DriverID         string `json:"DriverID"`
	CurrentStatus    string `json:"CurrentStatus"` // "Available" | "OnRoute" | "OffDuty"
	HoursDrivenToday int    `json:"HoursDrivenToday"`
	License          string `json:"License"`
}

type inMemoryStore struct {
	mu      sync.RWMutex
	drivers map[string]DriverState
}

type server struct {
	demoMode bool
	memStore *inMemoryStore   // nil when demoMode == false
	dynamoDB *dynamodb.Client // nil when demoMode == true
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
	mux.HandleFunc("POST /internal/v1/execute", srv.handleExecute)

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

	s.proxyToAgent(w, r.Context(), req.Prompt)
}

func (s *server) proxyToAgent(w http.ResponseWriter, ctx context.Context, cleanPrompt string) {
	sessionID := fmt.Sprintf("req-%d", time.Now().UnixNano())

	payload, _ := json.Marshal(AgentRequest{
		SessionID:   sessionID,
		CleanPrompt: cleanPrompt,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://localhost:5000/internal/v1/agent/process",
		bytes.NewReader(payload),
	)
	if err != nil {
		log.Printf("[AEGIS - PROXY] session=%s ERROR building request: %v", sessionID, err)
		writeJSON(w, http.StatusInternalServerError, SemanticRejection{
			Status:  "error",
			Reason:  "INTERNAL_ERROR",
			Message: "Failed to build upstream request.",
		})
		return
	}
	req.Header.Set("Content-Type", "application/json")

	log.Printf("[AEGIS - PROXY] session=%s → forwarding to agent", sessionID)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		// Distinguish connection-refused from timeout per spec §3.2.
		// Both cases protect the external client from seeing internal topology.
		var urlErr *url.Error
		if errors.As(err, &urlErr) && urlErr.Timeout() {
			log.Printf("[AEGIS - PROXY] session=%s TIMEOUT", sessionID)
			writeJSON(w, http.StatusGatewayTimeout, SemanticRejection{
				Status:  "error",
				Reason:  "GATEWAY_TIMEOUT",
				Message: "Agent did not respond within the allowed window.",
			})
		} else {
			log.Printf("[AEGIS - PROXY] session=%s UNAVAILABLE: %v", sessionID, err)
			writeJSON(w, http.StatusServiceUnavailable, SemanticRejection{
				Status:  "error",
				Reason:  "AGENT_UNAVAILABLE",
				Message: "Python agent is unreachable. Retry later.",
			})
		}
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
	log.Printf("[AEGIS - PROXY] session=%s ← agent responded status=%d", sessionID, resp.StatusCode)
}

func (s *server) handleExecute(w http.ResponseWriter, r *http.Request) {
	var intent ExecutionIntent
	if err := json.NewDecoder(r.Body).Decode(&intent); err != nil {
		writeJSON(w, http.StatusBadRequest, SemanticRejection{
			Status:  "blocked",
			Reason:  "INVALID_PAYLOAD",
			Message: "Request body must be a valid ExecutionIntent JSON.",
		})
		return
	}

	var driver DriverState
	var found bool

	if s.demoMode {
		driver, found = s.getDriverFromMemory(intent.DriverID)
	} else {
		var err error
		driver, found, err = s.getDriverFromDynamoDB(r.Context(), intent.DriverID)
		if err != nil {
			log.Printf("[AEGIS - POST-FLIGHT] ERROR fetching driver=%s: %v", intent.DriverID, err)
			writeJSON(w, http.StatusServiceUnavailable, SemanticRejection{
				Status:  "error",
				Reason:  "STORE_UNAVAILABLE",
				Message: "Failed to retrieve driver state from database.",
			})
			return
		}
	}

	if !found {
		writeJSON(w, http.StatusNotFound, SemanticRejection{
			Status:  "blocked",
			Reason:  "DRIVER_NOT_FOUND",
			Message: fmt.Sprintf("Driver %s does not exist in the system.", intent.DriverID),
		})
		return
	}

	// Regla 1 — Cuantitativa (Ley 18.290): acumulado diario no puede superar 5 horas.
	if driver.HoursDrivenToday+intent.EstimatedHours > 5 {
		log.Printf("[AEGIS - POST-FLIGHT] BLOCKED driver=%s rule=QUANTITATIVE total=%d",
			intent.DriverID, driver.HoursDrivenToday+intent.EstimatedHours)
		writeJSON(w, http.StatusBadRequest, SemanticRejection{
			Status:         "blocked",
			Reason:         "POLICY_VIOLATION_MATRIX",
			Message:        fmt.Sprintf("Driver %s rejected. Reason: Total hours would be %d, exceeds legal limit of 5 (Ley 18.290).", intent.DriverID, driver.HoursDrivenToday+intent.EstimatedHours),
			ActionRequired: fmt.Sprintf("Select a driver with hours_driven_today <= %d.", 5-intent.EstimatedHours),
		})
		return
	}

	// Regla 2 — Cualitativa (Certificación): licencia A5 requerida para todos los despachos.
	if driver.License != "A5" {
		log.Printf("[AEGIS - POST-FLIGHT] BLOCKED driver=%s rule=QUALITATIVE license=%s",
			intent.DriverID, driver.License)
		writeJSON(w, http.StatusBadRequest, SemanticRejection{
			Status:         "blocked",
			Reason:         "POLICY_VIOLATION_MATRIX",
			Message:        fmt.Sprintf("Driver %s rejected. Reason: Requires License A5, holds License %s.", intent.DriverID, driver.License),
			ActionRequired: "Select another driver with License == A5.",
		})
		return
	}

	// Regla 3 — Estado Operacional: solo conductores Available pueden recibir despachos.
	if driver.CurrentStatus != "Available" {
		log.Printf("[AEGIS - POST-FLIGHT] BLOCKED driver=%s rule=OPERATIONAL status=%s",
			intent.DriverID, driver.CurrentStatus)
		writeJSON(w, http.StatusBadRequest, SemanticRejection{
			Status:         "blocked",
			Reason:         "POLICY_VIOLATION_MATRIX",
			Message:        fmt.Sprintf("Driver %s rejected. Reason: Status is %s, must be Available.", intent.DriverID, driver.CurrentStatus),
			ActionRequired: "Select another driver with CurrentStatus == Available.",
		})
		return
	}

	// All rules passed — persist state mutation.
	if s.demoMode {
		s.updateDriverHoursInMemory(intent.DriverID, intent.EstimatedHours)
	} else {
		if err := s.updateDriverHoursInDynamoDB(r.Context(), intent.DriverID, intent.EstimatedHours); err != nil {
			log.Printf("[AEGIS - POST-FLIGHT] ERROR updating driver=%s: %v", intent.DriverID, err)
			writeJSON(w, http.StatusServiceUnavailable, SemanticRejection{
				Status:  "error",
				Reason:  "STORE_UNAVAILABLE",
				Message: "Failed to persist state mutation.",
			})
			return
		}
	}

	log.Printf("[AEGIS - POST-FLIGHT] APPROVED driver=%s destination=%s hours_added=%d new_total=%d",
		intent.DriverID, intent.Destination, intent.EstimatedHours, driver.HoursDrivenToday+intent.EstimatedHours)
	w.WriteHeader(http.StatusOK)
}

func (s *server) getDriverFromMemory(driverID string) (DriverState, bool) {
	s.memStore.mu.RLock()
	defer s.memStore.mu.RUnlock()
	driver, ok := s.memStore.drivers[driverID]
	return driver, ok
}

func (s *server) updateDriverHoursInMemory(driverID string, additionalHours int) {
	s.memStore.mu.Lock()
	defer s.memStore.mu.Unlock()
	driver := s.memStore.drivers[driverID]
	driver.HoursDrivenToday += additionalHours
	s.memStore.drivers[driverID] = driver
}

func (s *server) getDriverFromDynamoDB(ctx context.Context, driverID string) (DriverState, bool, error) {
	// 200 ms hard cap per spec §2.1 — gateway must fail fast on DB degradation.
	ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	out, err := s.dynamoDB.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String("AegisDrivers"),
		Key: map[string]dynamodbtypes.AttributeValue{
			"DriverID": &dynamodbtypes.AttributeValueMemberS{Value: driverID},
		},
	})
	if err != nil {
		return DriverState{}, false, err
	}
	if out.Item == nil {
		return DriverState{}, false, nil
	}

	var driver DriverState
	if err := attributevalue.UnmarshalMap(out.Item, &driver); err != nil {
		return DriverState{}, false, fmt.Errorf("unmarshal driver: %w", err)
	}
	return driver, true, nil
}

func (s *server) updateDriverHoursInDynamoDB(ctx context.Context, driverID string, additionalHours int) error {
	// 200 ms hard cap per spec §2.1.
	ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	_, err := s.dynamoDB.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String("AegisDrivers"),
		Key: map[string]dynamodbtypes.AttributeValue{
			"DriverID": &dynamodbtypes.AttributeValueMemberS{Value: driverID},
		},
		// ADD is atomic on DynamoDB numeric attributes — safe under concurrent writes.
		UpdateExpression: aws.String("ADD HoursDrivenToday :h"),
		ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
			":h": &dynamodbtypes.AttributeValueMemberN{Value: fmt.Sprintf("%d", additionalHours)},
		},
	})
	return err
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
