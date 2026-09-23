package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/sinthmux/sinthmux/internal/loginbroker"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "keygen" {
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			slog.Error("key generation failed", "error", err)
			os.Exit(1)
		}
		fmt.Printf("SINTHMUX_BROKER_SIGNING_SEED=%s\nSINTHMUX_AUTH_BROKER_PUBLIC_KEY=%s\n", base64.RawURLEncoding.EncodeToString(private.Seed()), base64.RawURLEncoding.EncodeToString(public))
		return
	}
	seed, err := base64.RawURLEncoding.DecodeString(os.Getenv("SINTHMUX_BROKER_SIGNING_SEED"))
	if err != nil || len(seed) != ed25519.SeedSize {
		slog.Error("SINTHMUX_BROKER_SIGNING_SEED must be a base64url Ed25519 seed")
		os.Exit(1)
	}
	server, err := loginbroker.New(loginbroker.Config{
		PublicURL:    os.Getenv("SINTHMUX_BROKER_PUBLIC_URL"),
		ClientID:     os.Getenv("SINTHMUX_GITHUB_CLIENT_ID"),
		ClientSecret: os.Getenv("SINTHMUX_GITHUB_CLIENT_SECRET"),
		SigningKey:   ed25519.NewKeyFromSeed(seed),
	})
	if err != nil {
		slog.Error("login broker configuration invalid", "error", err)
		os.Exit(1)
	}
	address := os.Getenv("SINTHMUX_BROKER_ADDR")
	if address == "" {
		address = "127.0.0.1:8091"
	}
	slog.Info("login broker listening", "address", address)
	if err := http.ListenAndServe(address, server.Handler()); err != nil {
		slog.Error("login broker stopped", "error", err)
		os.Exit(1)
	}
}
