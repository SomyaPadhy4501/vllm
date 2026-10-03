// Gateway: the single front door clients call. It looks up the
// requested model alias, picks a worker, and streams the answer back.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
)

func main() {
	port := flag.Int("port", 8080, "port to listen on")
	routesFile := flag.String("routes", "gateway/routes.json", "file mapping each model alias to its worker URLs")
	flag.Parse()

	// Routing table: alias -> list of worker URLs, loaded once at startup.
	data, err := os.ReadFile(*routesFile)
	if err != nil {
		log.Fatal(err)
	}
	var routes map[string][]string
	if err := json.Unmarshal(data, &routes); err != nil {
		log.Fatalf("bad routes file: %v", err)
	}

	http.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		// Read the whole body: we need the "model" field to choose a worker,
		// and then we forward the same bytes to that worker.
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "could not read request body (limit 1 MB)", http.StatusBadRequest)
			return
		}
		var req struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}

		workers := routes[req.Model]
		if len(workers) == 0 {
			http.Error(w, fmt.Sprintf("unknown model %q", req.Model), http.StatusNotFound)
			return
		}
		worker := workers[rand.IntN(len(workers))] // pick one at random

		// Build the same request for the worker. Using r.Context() means
		// that if the client goes away, this request is cancelled too.
		out, err := http.NewRequestWithContext(r.Context(), "POST", worker+"/v1/chat/completions", bytes.NewReader(body))
		if err != nil {
			http.Error(w, "bad worker URL", http.StatusInternalServerError)
			return
		}
		out.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(out)
		if err != nil {
			http.Error(w, "worker unreachable", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		// Pass the worker's status and headers (including its identity) back.
		for name, values := range resp.Header {
			w.Header()[name] = values
		}
		w.WriteHeader(resp.StatusCode)

		// Copy the answer piece by piece, flushing each piece right away
		// so tokens reach the client as soon as the worker sends them.
		rc := http.NewResponseController(w)
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
				rc.Flush()
			}
			if err != nil {
				return // worker finished (or the stream broke)
			}
		}
	})

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("gateway listening on %s with routes %v", addr, routes)
	log.Fatal(http.ListenAndServe(addr, nil))
}
