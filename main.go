package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/LaurensBosscher/read_the_room/internal/bypass"
	"github.com/LaurensBosscher/read_the_room/internal/fetch"
	"github.com/LaurensBosscher/read_the_room/internal/rules"
	"github.com/LaurensBosscher/read_the_room/internal/server"
	"github.com/LaurensBosscher/read_the_room/internal/update"
)

//go:embed config/rules.json
var builtinRules []byte

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	addr := flag.String("addr", envOr("READTHEROOM_ADDR", ":8080"), "listen address")
	rulesPath := flag.String("rules", envOr("READTHEROOM_RULES", "config/rules.json"), "rules file")
	dataDir := flag.String("data", envOr("READTHEROOM_DATA", "data"), "data directory")
	doUpdate := flag.Bool("update", false, "update upstream rules and exit")
	flag.Parse()

	store, err := rules.NewStore(*rulesPath, builtinRules, filepath.Join(*dataDir, "upstream"))
	if err != nil {
		log.Fatal(err)
	}
	updater := update.New(store, *dataDir)

	if *doUpdate {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if _, err := updater.Run(ctx); err != nil {
			log.Fatalf("update: %v", err)
		}
		failed := false
		for _, st := range updater.Status() {
			if st.LastError != "" {
				fmt.Printf("%s: error: %s\n", st.Name, st.LastError)
				failed = true
				continue
			}
			fmt.Printf("%s: ok (%d sites)\n", st.Name, st.Sites)
		}
		if failed {
			os.Exit(1)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go updater.Start(ctx)

	b := &bypass.Bypasser{
		Rules:   store,
		Fetcher: fetch.New(os.Getenv("READTHEROOM_ALLOW_PRIVATE") == "1"),
	}
	srv, err := server.New(store, updater, b)
	if err != nil {
		log.Fatal(err)
	}
	hs := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      90 * time.Second,
	}
	log.Printf("listening on %s", *addr)
	if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
