package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/adevireddy/colosseum/internal/api"
	"github.com/adevireddy/colosseum/internal/config"
	"github.com/adevireddy/colosseum/internal/db"
	"github.com/adevireddy/colosseum/internal/docker"
	"github.com/adevireddy/colosseum/internal/providers"
	"github.com/adevireddy/colosseum/internal/runtime"
	"github.com/adevireddy/colosseum/internal/tools"
	"github.com/joho/godotenv"
)

var version = "dev"

func main() {
	setupLogging()

	if len(os.Args) < 2 {
		fmt.Print(config.RootHelp())
		os.Exit(2)
	}

	switch os.Args[1] {
	case "server":
		runServer(os.Args[2:])
	case "help", "--help", "-h":
		if len(os.Args) > 2 && os.Args[2] == "server" {
			fmt.Print(config.ServerHelp())
			return
		}
		fmt.Print(config.RootHelp())
	case "version":
		fmt.Printf("colosseum %s\n", version)
	default:
		logErrorf("unknown command %q", os.Args[1])
		fmt.Fprintln(os.Stderr)
		fmt.Print(config.RootHelp())
		os.Exit(2)
	}
}

func runServer(args []string) {
	// Load optional .env files for local development without overriding shell env.
	_ = godotenv.Load(".env.local", ".env")
	if config.WantsHelp(args) {
		fmt.Print(config.ServerHelp())
		return
	}

	cfg, err := config.Load(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Print(config.ServerHelp())
			return
		}
		logErrorf("%v", err)
		fmt.Fprintln(os.Stderr)
		fmt.Print(config.ServerHelp())
		os.Exit(2)
	}
	logInfof(
		"startup version=%s bind=%s db=%s artifacts=%s workspace_root=%s browser_mode=%s",
		version, cfg.BindAddr, cfg.DBPath, cfg.ArtifactPath, cfg.WorkspaceRoot, cfg.BrowserMode,
	)
	if cfg.OpenAIKey == "" && cfg.AnthropicKey == "" {
		logWarnf("no model provider API keys configured; provider-backed runs may fail")
	}

	database, err := db.Open(cfg.DBPath)
	if err != nil {
		logFatalf("db open failed: %v", err)
	}
	defer database.Close()

	if err := db.Migrate(database); err != nil {
		logFatalf("migration failed: %v", err)
	}
	startupCtx, startupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer startupCancel()
	if err := tools.EnsureBuiltinDefinitions(startupCtx, database); err != nil {
		logFatalf("tool seed failed: %v", err)
	}

	if err := os.MkdirAll(cfg.ArtifactPath, 0o755); err != nil {
		logFatalf("artifact directory creation failed: %v", err)
	}
	if err := os.MkdirAll(cfg.WorkspaceRoot, 0o755); err != nil {
		logFatalf("workspace root creation failed: %v", err)
	}

	providerMap := map[string]providers.Client{}
	availableProviders := map[string]bool{}
	if cfg.AnthropicKey != "" {
		providerMap["anthropic"] = &providers.AnthropicClient{APIKey: cfg.AnthropicKey}
		availableProviders["anthropic"] = true
	}
	if cfg.OpenAIKey != "" {
		providerMap["openai"] = &providers.OpenAIClient{APIKey: cfg.OpenAIKey}
		availableProviders["openai"] = true
	}
	if cfg.DeepSeekKey != "" {
		providerMap["deepseek"] = &providers.OpenAIClient{APIKey: cfg.DeepSeekKey, BaseURL: "https://api.deepseek.com"}
		availableProviders["deepseek"] = true
	}
	dockerMgr, err := docker.NewManager(database, cfg.DockerImage)
	if err != nil {
		logFatalf("docker init failed: %v", err)
	}
	dockerPingCtx, dockerPingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := dockerMgr.Ping(dockerPingCtx); err != nil {
		dockerPingCancel()
		logWarnf("docker ping failed: %v", err)
	} else {
		dockerPingCancel()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := dockerMgr.CleanupOrphans(cleanupCtx); err != nil {
			logWarnf("docker cleanup orphans failed: %v", err)
		}
		cleanupCancel()
	}

	toolExec := &tools.Executor{DB: database, ArtifactsDir: cfg.ArtifactPath, Docker: dockerMgr}
	toolExec.Browser = &tools.BrowserRuntime{
		Mode:     cfg.BrowserMode,
		Image:    cfg.BrowserImage,
		Fallback: cfg.BrowserFallback,
	}
	runtimeMgr := runtime.NewManager(database, providerMap, toolExec, cfg.SecretKey, cfg.DockerImage)
	runtimeCtx, runtimeCancel := context.WithCancel(context.Background())
	defer runtimeCancel()
	go runtimeMgr.Start(runtimeCtx)

	srv := api.NewServer(database, cfg.WorkspaceRoot, availableProviders, cfg.OpenAIKey, cfg.AnthropicKey, cfg.SecretKey, cfg.APIAuthToken, providerMap, cfg.BasicAuthUser, cfg.BasicAuthPass)
	httpServer := &http.Server{
		Addr:              cfg.BindAddr,
		Handler:           srv.Handler(),
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	go func() {
		logInfof("listening bind=%s", cfg.BindAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logFatalf("http server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 2)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(stop)

	firstSignal := <-stop
	logWarnf("shutdown signal=%s received; stopping server", firstSignal.String())

	// Second Ctrl+C forces immediate exit.
	go func() {
		<-stop
		logErrorf("second shutdown signal received; forcing exit")
		os.Exit(1)
	}()

	runtimeCancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runtimeMgr.Wait()
	}()

	waitDone := make(chan struct{})
	go func() {
		defer close(waitDone)
		wg.Wait()
	}()

	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		logWarnf("runtime workers still stopping; continuing with HTTP shutdown")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		logWarnf("http shutdown warning: %v", err)
	}
	logInfof("server stopped cleanly")
}

func setupLogging() {
	log.SetOutput(os.Stdout)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.LUTC)
}

func logInfof(format string, args ...any) {
	log.Printf("level=INFO msg=\""+format+"\"", args...)
}

func logWarnf(format string, args ...any) {
	log.Printf("level=WARN msg=\""+format+"\"", args...)
}

func logErrorf(format string, args ...any) {
	log.Printf("level=ERROR msg=\""+format+"\"", args...)
}

func logFatalf(format string, args ...any) {
	log.Printf("level=FATAL msg=\""+format+"\"", args...)
	os.Exit(1)
}
