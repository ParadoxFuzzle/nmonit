// Command control-plane is the nmonit control-plane daemon.
//
// It exposes two surfaces:
//
//   - gRPC AgentService on --grpc-addr (default :9000) for registered agents.
//   - HTTP REST API on --http-addr (default :8080) for /health, /api/v1/nodes,
//     and /metrics.
//
// Domain code lives in internal packages:
//
//   - internal/registry — node state, heartbeat accounting, stale cleanup.
//   - internal/agent    — gRPC handlers + auth + input validation.
//   - internal/restapi  — HTTP handlers, response shapes, summary mappings.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	computev1 "github.com/ParadoxFuzzle/control-plane/gen/compute/v1"
	"github.com/ParadoxFuzzle/control-plane/internal/agent"
	"github.com/ParadoxFuzzle/control-plane/internal/metrics"
	"github.com/ParadoxFuzzle/control-plane/internal/registry"
	"github.com/ParadoxFuzzle/control-plane/internal/restapi"
	"github.com/ParadoxFuzzle/control-plane/internal/tlsreload"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
)

func main() {
	var (
		grpcAddr          = flag.String("grpc-addr", ":9000", "gRPC listen address")
		httpAddr          = flag.String("http-addr", ":8080", "REST API listen address")
		nodeID            = flag.String("node-id", "", "Node ID (defaults to hostname)")
		logLevel          = flag.String("log-level", "info", "Log level")
		bootstrap         = flag.Bool("bootstrap", false, "Bootstrap new cluster")
		agentToken        = flag.String("agent-token", "", "Shared secret for agent authentication (empty = no auth)")
		apiKey            = flag.String("api-key", "", "API key for REST auth (empty = no auth)")
		tlsCert           = flag.String("tls-cert", "", "Path to TLS server certificate (PEM)")
		tlsKey            = flag.String("tls-key", "", "Path to TLS server private key (PEM)")
		tlsCACert         = flag.String("tls-ca-cert", "", "Path to CA certificate for mTLS (PEM). If set, requires client certs.")
		requireTLS        = flag.Bool("require-tls", false, "Refuse startup unless TLS is configured (production safety)")
		tlsReloadInterval = flag.Duration("tls-reload-interval", 60*time.Second, "Interval for polling TLS cert files for changes (0 = disable hot-reload)")
	)
	flag.Parse()

	level, _ := zerolog.ParseLevel(*logLevel)
	zerolog.SetGlobalLevel(level)
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

	if *nodeID == "" {
		h, _ := os.Hostname()
		*nodeID = h
	}

	clusterID := "cluster-001"
	if *bootstrap {
		clusterID = "cluster-" + uuid.NewString()
	}

	leaderAddr := *grpcAddr
	if leaderAddr == ":9000" || leaderAddr == "" {
		leaderAddr = "localhost:9000"
	}

	log.Info().
		Str("node_id", *nodeID).
		Str("grpc_addr", *grpcAddr).
		Str("http_addr", *httpAddr).
		Bool("bootstrap", *bootstrap).
		Bool("auth_enabled", *agentToken != "" || *apiKey != "").
		Str("cluster_id", clusterID).
		Msg("compute-control-plane starting")

	reg := registry.NewNodeRegistry()

	cleanupCtx, cleanupCancel := context.WithCancel(context.Background())
	defer cleanupCancel()
	go reg.StaleNodeCleanup(cleanupCtx, 30*time.Second)

	grpcLis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to listen gRPC")
	}

	var tlsEnabled bool
	switch {
	case *tlsCert == "" && *tlsKey == "":
		if *requireTLS {
			log.Fatal().Msg("--require-tls is set but no TLS credentials were provided; refusing to start with plaintext")
		}
		tlsEnabled = false
	case *tlsCert == "" || *tlsKey == "":
		log.Fatal().Msg("both --tls-cert and --tls-key must be provided together")
	default:
		tlsEnabled = true
	}

	if !tlsEnabled {
		log.Warn().Msg("TLS not configured — gRPC running with plaintext (insecure)")
	}

	var reloader *tlsreload.CertReloader
	if tlsEnabled {
		var err error
		reloader, err = tlsreload.New(*tlsCert, *tlsKey, *tlsCACert)
		if err != nil {
			log.Fatal().Err(err).Msg("failed to load TLS credentials")
		}

		log.Info().
			Bool("mtls", *tlsCACert != "").
			Dur("reload_interval", *tlsReloadInterval).
			Msg("TLS enabled for gRPC + HTTPS servers")

		if *tlsReloadInterval > 0 {
			go reloader.Watch(context.Background(), *tlsReloadInterval)
		}
	}

	grpcOpts := []grpc.ServerOption{
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle:     5 * time.Minute,
			MaxConnectionAge:      30 * time.Minute,
			MaxConnectionAgeGrace: 10 * time.Second,
			Time:                  10 * time.Second,
			Timeout:               5 * time.Second,
		}),
		// Chain ordering is significant: grpc.ChainUnaryInterceptor runs
		// the leftmost interceptor first, on the outermost layer. This
		// ordering records metrics AROUND the auth interceptor, so
		// auth-rejected calls (Unauthenticated / PermissionDenied) STILL
		// appear in the request-counter AND the request-latency histogram.
		// Swapping the order would silently make auth abuse invisible on
		// the dashboard. Enforced by interceptor_test.go.
		grpc.ChainUnaryInterceptor(
			metrics.UnaryServerInterceptor(),
			agent.AuthUnaryInterceptorFor(*agentToken),
		),
		grpc.StreamInterceptor(metrics.StreamServerInterceptor()),
	}
	if tlsEnabled {
		grpcCfg := &tls.Config{
			MinVersion:         tls.VersionTLS13,
			GetConfigForClient: reloader.GetConfigForClient,
		}
		grpcOpts = append(grpcOpts, grpc.Creds(credentials.NewTLS(grpcCfg)))
	}

	grpcServer := grpc.NewServer(grpcOpts...)
	agentServer := agent.NewAgentServer(reg, clusterID, leaderAddr, *agentToken)
	computev1.RegisterAgentServiceServer(grpcServer, agentServer)

	go func() {
		log.Info().Str("addr", *grpcAddr).Msg("gRPC server starting")
		if err := grpcServer.Serve(grpcLis); err != nil {
			log.Fatal().Err(err).Msg("gRPC server failed")
		}
	}()

	api := restapi.NewAPIServer(reg, *apiKey, tlsEnabled)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", api.HealthHandler)
	mux.HandleFunc("/api/v1/nodes", api.RequireAPIKey(api.NodesHandler))
	mux.Handle("/metrics", api.RequireAPIKey(metrics.Handler().ServeHTTP))

	httpServer := &http.Server{
		Addr:              *httpAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if tlsEnabled {
		httpServer.TLSConfig = &tls.Config{
			MinVersion:     tls.VersionTLS13,
			GetCertificate: reloader.GetCertificate,
		}
	}

	go func() {
		if tlsEnabled {
			log.Info().Str("addr", *httpAddr).Msg("HTTPS API starting")
			// Pass empty cert/key — the server uses TLSConfig.GetCertificate.
			if err := httpServer.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
				log.Fatal().Err(err).Msg("HTTPS server failed")
			}
		} else {
			log.Info().Str("addr", *httpAddr).Msg("HTTP API starting (plaintext)")
			if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatal().Err(err).Msg("HTTP server failed")
			}
		}
	}()

	log.Info().Msg("compute-control-plane initialized and ready")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
shutdown:
	for {
		sig := <-sigCh
		switch sig {
		case syscall.SIGHUP:
			if reloader != nil {
				log.Info().Msg("SIGHUP received, reloading TLS certificates")
				reloader.Reload()
			}
			continue
		default:
			log.Info().Str("signal", sig.String()).Msg("shutting down")
			break shutdown
		}
	}

	grpcServer.GracefulStop()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Warn().Err(err).Msg("HTTP server forced to shutdown")
	}

	log.Info().Msg("shutdown complete")
}
