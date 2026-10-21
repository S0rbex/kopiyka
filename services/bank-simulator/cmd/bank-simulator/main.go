// Command bank-simulator runs the fake acquiring bank used by Kopiyka.
package main

import (
	"log"
	"net/http"
	"os"
)

func main() {
	addr := ":" + envOr("PORT", "9090")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"UP"}`))
	})

	log.Printf("bank-simulator listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
