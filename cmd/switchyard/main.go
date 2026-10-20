// Command switchyard runs the gateway.
//
//	switchyard serve  -config switchyard.yaml   run the gateway (default)
//	switchyard check  -config switchyard.yaml   validate a config and exit
//	switchyard keygen -name ci                  create a client key
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shreyashyadav1/switchyard/internal/auth"
	"github.com/shreyashyadav1/switchyard/internal/config"
	"github.com/shreyashyadav1/switchyard/internal/proxy"
	"github.com/shreyashyadav1/switchyard/internal/telemetry"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "check":
		err = check(args)
	case "keygen":
		err = keygen(args)
	case "version":
		fmt.Println(version)
	default:
		err = fmt.Errorf("unknown command %q (want serve, check, keygen or version)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "switchyard:", err)
		os.Exit(1)
	}
}

func configFlag(fs *flag.FlagSet) *string {
	def := os.Getenv("SWITCHYARD_CONFIG")
	if def == "" {
		def = "switchyard.yaml"
	}
	return fs.String("config", def, "path to the config file (env SWITCHYARD_CONFIG)")
}

func check(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	path := configFlag(fs)
	_ = fs.Parse(args)
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	fmt.Printf("%s: %d providers, %d models, %d keys\n", *path, len(cfg.Providers), len(cfg.Models), len(cfg.Keys))
	return nil
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	name := fs.String("name", "", "key name, shown in logs and metrics")
	_ = fs.Parse(args)
	if *name == "" {
		return errors.New("keygen: -name is required")
	}
	token, digest, err := auth.NewToken()
	if err != nil {
		return err
	}
	fmt.Printf("Token (give this to the client; it is not stored anywhere):\n  %s\n\n", token)
	fmt.Printf("Config entry:\n  - name: %s\n    sha256: %s\n", *name, digest)
	return nil
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	path := configFlag(fs)
	metricsAddr := fs.String("metrics-listen", envOr("SWITCHYARD_METRICS_LISTEN", "127.0.0.1:9090"),
		"address for the Prometheus /metrics listener; empty disables it")
	_ = fs.Parse(args)

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	if cfg.AllowAnonymous {
		log.Warn("anonymous access is enabled: requests without a key are accepted")
	}
	// Cloud Run and similar platforms tell the process which port to use.
	listen := cfg.Listen
	if port := os.Getenv("PORT"); port != "" {
		listen = ":" + port
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := telemetry.SetupTracing(ctx, version)
	if err != nil {
		return fmt.Errorf("tracing: %w", err)
	}
	gw := proxy.New(cfg, telemetry.NewMetrics(), log)

	servers := []*http.Server{newServer(listen, gw.Handler())}
	if *metricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", gw.MetricsHandler())
		servers = append(servers, newServer(*metricsAddr, mux))
	}

	errc := make(chan error, len(servers))
	for _, s := range servers {
		ln, err := net.Listen("tcp", s.Addr)
		if err != nil {
			return err
		}
		log.Info("listening", "addr", ln.Addr().String(), "version", version)
		go func() { errc <- s.Serve(ln) }()
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdownCtx)
	}
	return shutdownTracing(shutdownCtx)
}

func newServer(addr string, h http.Handler) *http.Server {
	// No WriteTimeout: streamed completions can run for minutes. The
	// gateway's request_timeout bounds each request instead.
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}
