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
	"sync"
)

// loadRoutes reads the routing table: alias -> list of worker URLs.
func loadRoutes(path string) (map[string][]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var routes map[string][]string
	if err := json.Unmarshal(data, &routes); err != nil {
		return nil, fmt.Errorf("bad routes file: %w", err)
	}
	return routes, nil
}

func main() {
	port := flag.Int("port", 8080, "port to listen on")
	routesFile := flag.String("routes", "gateway/routes.json", "file mapping each model alias to its worker URLs")
	flag.Parse()

	routes, err := loadRoutes(*routesFile)
	if err != nil {
		log.Fatal(err)
	}
	// Many requests read the table while a reload may replace it,
	// so every read and write of "routes" happens while holding mu.
	var mu sync.Mutex

	// Re-read routes.json without restarting. The whole table is swapped
	// at once; if the new file is bad, the old table stays in use.
	http.HandleFunc("POST /admin/reload", func(w http.ResponseWriter, r *http.Request) {
		newRoutes, err := loadRoutes(*routesFile)
		if err != nil {
			http.Error(w, err.Error()+" (keeping old routes)", http.StatusInternalServerError)
			return
		}
		mu.Lock()
		routes = newRoutes
		mu.Unlock()
		log.Printf("routes reloaded: %v", newRoutes)
		fmt.Fprintln(w, "routes reloaded")
	})

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

		mu.Lock()
		workers := routes[req.Model]
		mu.Unlock()
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
	log.Printf("gateway listening on %s with routes %v", addr, routes) // no lock needed: no requests yet
	log.Fatal(http.ListenAndServe(addr, nil))
}
