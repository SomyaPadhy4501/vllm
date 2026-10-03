// Mock inference worker: stands in for a vLLM worker so the gateway
// can be built and tested without a GPU.
package main

import (
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	mrand "math/rand/v2"
	"net/http"
	"time"
)

// Only the fields the mock needs; other fields (messages, etc.) are ignored.
type chatRequest struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
}

func main() {
	port := flag.Int("port", 9001, "port to listen on")
	deployment := flag.String("deployment", "A", "deployment ID this worker belongs to")
	worker := flag.String("worker", "A1", "worker ID")
	maxTokens := flag.Int("max-tokens", 256, "largest max_tokens a request may ask for")
	tokenDelay := flag.Duration("token-delay", 50*time.Millisecond, "delay before each token")
	startupDelay := flag.Duration("startup-delay", 0, "pretend model loading time before the worker is ready")
	errorRate := flag.Float64("error-rate", 0, "fraction of chat requests (0 to 1) that fail with a 500 error")
	breakAfter := flag.Int("break-after", 0, "cut every stream off after this many tokens (0 = never)")
	flag.Parse()

	// New random ID every time the process starts, so a restarted
	// worker is never mistaken for its previous self.
	incarnation := rand.Text()

	// Pretend model loading: the port is open right away,
	// but the worker can't serve until this moment.
	readyAt := time.Now().Add(*startupDelay)

	http.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		status := "ready"
		if time.Now().Before(readyAt) {
			status = "starting"
			w.WriteHeader(http.StatusServiceUnavailable) // 503: running, but not ready
		}
		json.NewEncoder(w).Encode(map[string]string{
			"deployment_id": *deployment,
			"worker_id":     *worker,
			"incarnation":   incarnation,
			"status":        status,
		})
	})

	http.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if time.Now().Before(readyAt) {
			http.Error(w, "worker is still starting", http.StatusServiceUnavailable)
			return
		}
		var req chatRequest
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // reject bodies over 1 MB
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		if req.MaxTokens == 0 {
			req.MaxTokens = 16
		}
		if req.MaxTokens < 0 || req.MaxTokens > *maxTokens {
			http.Error(w, fmt.Sprintf("max_tokens must be between 1 and %d", *maxTokens), http.StatusBadRequest)
			return
		}

		// Failure injection: fail some requests on purpose, before any token is sent.
		if mrand.Float64() < *errorRate {
			http.Error(w, "injected failure", http.StatusInternalServerError)
			return
		}

		// Identity goes in headers so the gateway can record who served the request.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Deployment-ID", *deployment)
		w.Header().Set("X-Worker-ID", *worker)
		w.Header().Set("X-Incarnation", incarnation)
		rc := http.NewResponseController(w)

		for i := 1; i <= req.MaxTokens; i++ {
			// Wait for the next token, unless the client has gone away.
			select {
			case <-r.Context().Done():
				log.Printf("request cancelled after %d tokens", i-1)
				return
			case <-time.After(*tokenDelay):
			}
			fmt.Fprintf(w, `data: {"choices":[{"delta":{"content":"token%d "}}]}`+"\n\n", i)
			rc.Flush() // send this token now instead of buffering

			if i == *breakAfter {
				log.Printf("injected stream break after %d tokens", i)
				return // stop without [DONE], like a crashed worker
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("worker %s (deployment %s, incarnation %s) listening on %s", *worker, *deployment, incarnation, addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
