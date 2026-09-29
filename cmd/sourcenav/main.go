// Command sourcenav runs the first-version source navigation service for the
// Edulang teaching language: cross-file definition lookup and references.
package main

import (
	"flag"
	"log"
	"net/http"

	"sourcenav/internal/server"
	"sourcenav/internal/workspace"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	h := server.New(workspace.New())
	log.Printf("sourcenav listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, h.Mux()))
}
