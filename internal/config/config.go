package config

import "os"

type Hub struct {
	Address            string
	DevToken           string
	DatabaseURL        string
	PublicURL          string
	GithubClientID     string
	GithubClientSecret string
}

type Agent struct {
	HubURL      string
	DevToken    string
	DeviceToken string
	DeviceID    string
	Name        string
}

func HubFromEnv() Hub {
	return Hub{
		Address:            env("SINTHMUX_HUB_ADDR", "127.0.0.1:8090"),
		DevToken:           env("SINTHMUX_DEV_TOKEN", "sinthmux-local-dev"),
		DatabaseURL:        os.Getenv("SINTHMUX_DATABASE_URL"),
		PublicURL:          os.Getenv("SINTHMUX_PUBLIC_URL"),
		GithubClientID:     os.Getenv("SINTHMUX_GITHUB_CLIENT_ID"),
		GithubClientSecret: os.Getenv("SINTHMUX_GITHUB_CLIENT_SECRET"),
	}
}

func AgentFromEnv() Agent {
	return Agent{
		HubURL:      env("SINTHMUX_AGENT_HUB_URL", "ws://127.0.0.1:8090/ws/v1/agents/connect"),
		DevToken:    env("SINTHMUX_DEV_TOKEN", "sinthmux-local-dev"),
		DeviceToken: os.Getenv("SINTHMUX_AGENT_DEVICE_TOKEN"),
		DeviceID:    env("SINTHMUX_AGENT_DEVICE_ID", "local-dev"),
		Name:        env("SINTHMUX_AGENT_NAME", "Local Development Machine"),
	}
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
