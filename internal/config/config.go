package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

type Config struct {
	BindAddr        string
	ListenIP        string
	Port            int
	DBPath          string
	ArtifactPath    string
	WorkspaceRoot   string
	DockerHost      string
	DockerImage     string
	OpenAIKey       string
	AnthropicKey    string
	DeepSeekKey     string
	APIAuthToken    string
	SecretKey       string
	BasicAuthUser   string
	BasicAuthPass   string
	DefaultModel    string
	BrowserMode     string
	BrowserImage    string
	BrowserFallback bool
}

func Load(args []string) (Config, error) {
	listenIP := getenv("COLOSSEUM_LISTEN_IP", "0.0.0.0")
	port := getenvInt("COLOSSEUM_PORT", 8080)
	defaultBind := getenv("COLOSSEUM_BIND", fmt.Sprintf("%s:%d", listenIP, port))

	cfg := Config{
		BindAddr:        defaultBind,
		ListenIP:        listenIP,
		Port:            port,
		DBPath:          getenv("COLOSSEUM_DB_PATH", "./colosseum.db"),
		ArtifactPath:    getenv("COLOSSEUM_ARTIFACT_PATH", "./artifacts"),
		WorkspaceRoot:   getenv("COLOSSEUM_WORKSPACE_ROOT", "./workspaces"),
		DockerHost:      getenv("DOCKER_HOST", ""),
		DockerImage:     getenv("COLOSSEUM_DOCKER_IMAGE", "python:3.12"),
		OpenAIKey:       os.Getenv("OPENAI_API_KEY"),
		AnthropicKey:    os.Getenv("ANTHROPIC_API_KEY"),
		DeepSeekKey:     os.Getenv("DEEPSEEK_API_KEY"),
		APIAuthToken:    os.Getenv("COLOSSEUM_API_AUTH_TOKEN"),
		SecretKey:       os.Getenv("COLOSSEUM_SECRET_KEY"),
		BasicAuthUser:   os.Getenv("COLOSSEUM_BASIC_AUTH_USER"),
		BasicAuthPass:   os.Getenv("COLOSSEUM_BASIC_AUTH_PASS"),
		DefaultModel:    getenv("COLOSSEUM_DEFAULT_MODEL", "gpt-4.1-mini"),
		BrowserMode:     getenv("COLOSSEUM_BROWSER_MODE", "docker"),
		BrowserImage:    getenv("COLOSSEUM_BROWSER_IMAGE", "mcr.microsoft.com/playwright:v1.59.1-jammy"),
		BrowserFallback: getenvBool("COLOSSEUM_BROWSER_FALLBACK", true),
	}

	fs := flag.NewFlagSet("colosseum server", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.BindAddr, "bind", cfg.BindAddr, "HTTP bind address")
	fs.StringVar(&cfg.ListenIP, "listen-ip", cfg.ListenIP, "HTTP listen IP address")
	fs.IntVar(&cfg.Port, "port", cfg.Port, "HTTP listen port")
	fs.StringVar(&cfg.DBPath, "db", cfg.DBPath, "SQLite database path")
	fs.StringVar(&cfg.ArtifactPath, "artifacts", cfg.ArtifactPath, "artifact directory")
	fs.StringVar(&cfg.WorkspaceRoot, "workspace-root", cfg.WorkspaceRoot, "managed workspace root directory")
	fs.StringVar(&cfg.DefaultModel, "model", cfg.DefaultModel, "default model")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return cfg, flag.ErrHelp
		}
		return Config{}, fmt.Errorf("invalid server flags: %w", err)
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " "))
	}
	if !flagWasSet(fs, "bind") {
		cfg.BindAddr = fmt.Sprintf("%s:%d", cfg.ListenIP, cfg.Port)
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		return Config{}, fmt.Errorf("invalid port %d: must be between 1 and 65535", cfg.Port)
	}
	if strings.TrimSpace(cfg.BindAddr) == "" {
		return Config{}, fmt.Errorf("bind address cannot be empty")
	}
	switch strings.TrimSpace(strings.ToLower(cfg.BrowserMode)) {
	case "docker", "local":
	default:
		return Config{}, fmt.Errorf("invalid COLOSSEUM_BROWSER_MODE %q: expected docker or local", cfg.BrowserMode)
	}

	return cfg, nil
}

func getenv(key, fallback string) string {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	return v
}

func getenvInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	var parsed int
	_, err := fmt.Sscanf(v, "%d", &parsed)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func flagWasSet(fs *flag.FlagSet, flagName string) bool {
	seen := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == flagName {
			seen = true
		}
	})
	return seen
}

func RootHelp() string {
	return `colosseum - self-hosted agent runtime and operations console

Usage:
  colosseum <command> [flags]

Commands:
  server    Start API/runtime server
  help      Show help
  version   Show version

Run "colosseum help server" for server command details.
`
}

func ServerHelp() string {
	return `colosseum server - start the API/runtime server

Usage:
  colosseum server [flags]

Flags:
  --bind <ip:port>           Full bind address (overrides --listen-ip/--port)
  --listen-ip <ip>           Listen IP/host (default: 0.0.0.0)
  --port <port>              Listen port (default: 8080)
  --db <path>                SQLite database path (default: ./colosseum.db)
  --artifacts <path>         Artifact directory (default: ./artifacts)
  --workspace-root <path>    Managed workspace root (default: ./workspaces)
  --model <name>             Default model fallback (default: gpt-4.1-mini)
  -h, --help                 Show this help

Environment:
  OPENAI_API_KEY, ANTHROPIC_API_KEY, DEEPSEEK_API_KEY
  COLOSSEUM_API_AUTH_TOKEN, COLOSSEUM_SECRET_KEY
  COLOSSEUM_BIND, COLOSSEUM_LISTEN_IP, COLOSSEUM_PORT
  COLOSSEUM_DB_PATH, COLOSSEUM_ARTIFACT_PATH, COLOSSEUM_WORKSPACE_ROOT
  COLOSSEUM_DEFAULT_MODEL, COLOSSEUM_DOCKER_IMAGE, DOCKER_HOST
  COLOSSEUM_BROWSER_MODE, COLOSSEUM_BROWSER_IMAGE, COLOSSEUM_BROWSER_FALLBACK

Notes:
  - Environment variables can be passed via shell or loaded from .env.local/.env.
  - Precedence: flags > shell env > .env.local/.env > built-in defaults.
  - Default listener is 0.0.0.0 for production-friendly container/server use.

Examples:
  colosseum server
  colosseum server --port 8001
  OPENAI_API_KEY=... colosseum server --db ./colosseum.db
`
}

func WantsHelp(args []string) bool {
	for i := range args {
		if args[i] == "-h" || args[i] == "--help" {
			return true
		}
	}
	return false
}

func getenvBool(key string, fallback bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if v == "" {
		return fallback
	}
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}
