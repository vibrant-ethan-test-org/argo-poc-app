// Command release-descriptor is the publisher behind the shared release-descriptor GitHub Action
// (docs/design/23-release-descriptors.md §23.4). It reads its inputs from the environment the action sets, prints
// progress and at most one fixed error line, and writes outputs only after a verified publication.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Vibrant-America/promotion-controller/internal/descriptor/publish"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	opts, err := publish.OptionsFromEnv(os.Getenv)
	if err == nil {
		_, err = publish.Publish(ctx, opts, publish.Deps{Source: publish.GitSource{}, Log: os.Stdout})
	}
	if err != nil {
		fmt.Println("::error::" + publish.CommandEscape(publish.Message(err)))
		cancel()
		stop()
		os.Exit(1)
	}
}
