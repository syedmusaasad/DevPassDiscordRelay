// cmd/relay: main entry point for the devpass-discord relay.
// This is a scaffold; full implementation is in future phases.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	log.Println("relay: starting (scaffold)")

	<-ctx.Done()
	log.Println("relay: shutting down")
}
