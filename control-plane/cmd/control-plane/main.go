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
	"database/sql"
	"flag"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ParadoxFuzzle/control-plane/internal/audit"
	"github.com/ParadoxFuzzle/control-plane/internal/identity"
	_ "github.com/jackc/pgx/v5/stdlib"

	computev1 "github.com/ParadoxFuzzle/control-plane/gen/compute/v1"
	"github.com/ParadoxFuzzle/control-plane/internal/agent"
	"github.com/ParadoxFuzzle/control-plane/internal/jobs"
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
		pgDSN             = flag.String("pg-dsn", "", "Postgres DSN for the identity layer (e.g. postgres://user:pass@host:5432/db). Empty = legacy shared-token mode.")

		// Phase 2 job quotas (defaults when a user has no override).
		maxJobsRunning = flag.Int("max-jobs-running", 2, "Default max running+scheduling jobs per consumer")
		maxJobsQueued  = flag.Int("max-jobs-queued", 5, "Default max queued jobs per consumer")
		maxJobCores    = flag.Float64("max-job-cores", 4, "Max cpu_cores a single consumer job may request")
		maxJobMemGB    = flag.Float64("max-job-mem-gb", 8, "Max memory (GiB) a single consumer job may request")
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

	// Identity layer (Phase 1 public-access): optional. When --pg-dsn is set,
	// agents authenticate with per-node tokens on top of the shared secret.
	var (
		identityStore identity.Store
		aud           *audit.Audit
		auth          *agent.Authenticator
	)
	if *pgDSN != "" {
		pg, err := identity.NewPGStore(*pgDSN)
		if err != nil {
			log.Fatal().Err(err).Msg("identity store unavailable")
		}
		defer pg.Close()
		identityStore = pg
		log.Info().Msg("identity layer enabled (per-node tokens, invites, audit)")
	}
	aud = audit.New(nil)
	if *pgDSN != "" {
		if db, err := sql.Open("pgx", *pgDSN); err != nil {
			log.Fatal().Err(err).Msg("audit db open failed")
		} else {
			aud = audit.New(db)
		}
	}
	auth = agent.NewAuthenticator(*agentToken, identityStore, aud)
	auth.SetRateLimiter(20, 40) // per-credential: 20 req/s, burst 40

	// Phase 2 job-layer config (store/hub/scheduler are created after the
	// node registry below).
	quotaDefaults := jobs.QuotaDefaults{
		MaxRunning:     *maxJobsRunning,
		MaxQueued:      *maxJobsQueued,
		MaxJobCores:    float32(*maxJobCores),
		MaxJobMemBytes: uint64(*maxJobMemGB * float64(1<<30)),
	}
	var (
		jobStore    jobs.Store
		dispatchHub *jobs.Hub
		scheduler   *jobs.Scheduler
	)

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

	// Phase 2 job layer: same Postgres DB as the identity layer. Enabled
	// whenever --pg-dsn is set; hub feeds TaskSpecs to agents through their
	// heartbeat streams, scheduler assigns queued jobs best-fit.
	if *pgDSN != "" {
		db, err := sql.Open("pgx", *pgDSN)
		if err != nil {
			log.Fatal().Err(err).Msg("job store db open failed")
		}
		js, err := jobs.NewPGStore(db)
		if err != nil {
			log.Fatal().Err(err).Msg("job store unavailable")
		}
		jobStore = js
		dispatchHub = jobs.NewHub(jobStore, aud)
		scheduler = jobs.NewScheduler(dispatchHub, reg, jobStore, aud,
			quotaDefaults.MaxJobCores, quotaDefaults.MaxJobMemBytes)
		log.Info().
			Int("max_jobs_running", quotaDefaults.MaxRunning).
			Int("max_jobs_queued", quotaDefaults.MaxQueued).
			Float64("max_job_cores", *maxJobCores).
			Float64("max_job_mem_gb", *maxJobMemGB).
			Msg("job layer enabled (submit, quotas, best-fit dispatch)")
	}

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
		// The Rust agent (tonic) sends HTTP/2 pings every 10s
		// (http2_keep_alive_interval). gRPC-Go's default enforcement policy
		// expects client pings no more often than 5 minutes and kills the
		// connection with GOAWAY ("got too many pings") otherwise — which
		// silently evicted every agent ~40s in. Align enforcement with the
		// server's own Time (10s) so agent keep-alive pings are accepted.
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
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
			auth.UnaryInterceptor(),
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
	agentServer := agent.NewAgentServerWithIdentity(reg, clusterID, leaderAddr, auth, aud)
	agentServer.SetJobDispatch(dispatchHub, scheduler)
	computev1.RegisterAgentServiceServer(grpcServer, agentServer)
	if jobStore != nil {
		jobServer := agent.NewJobServer(auth, jobStore, identityStore, dispatchHub, scheduler, aud, quotaDefaults)
		computev1.RegisterJobServiceServer(grpcServer, jobServer)
	}

	go func() {
		log.Info().Str("addr", *grpcAddr).Msg("gRPC server starting")
		if err := grpcServer.Serve(grpcLis); err != nil {
			log.Fatal().Err(err).Msg("gRPC server failed")
		}
	}()
	api := restapi.NewAPIServer(reg, *apiKey, tlsEnabled)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(landingHTML))
	})
	mux.HandleFunc("/health", api.HealthHandler)
	mux.HandleFunc("/api/v1/nodes", api.RequireAPIKey(api.NodesHandler))
	mux.Handle("/metrics", api.RequireAPIKey(metrics.Handler().ServeHTTP))
	if identityStore != nil {
		ih := restapi.NewIdentityHandlers(identityStore, aud, *apiKey)
		mux.HandleFunc("/join", ih.JoinHandler)
		mux.HandleFunc("/api/v1/admin/users", ih.AdminUsersHandler)
		mux.HandleFunc("/api/v1/admin/users/", ih.AdminUserStatusHandler)
		mux.HandleFunc("/api/v1/admin/tokens", ih.AdminTokensHandler)
		mux.HandleFunc("/api/v1/admin/tokens/", ih.AdminTokenRevokeHandler)
		mux.HandleFunc("/api/v1/admin/invites", ih.AdminInvitesHandler)
		mux.HandleFunc("/api/v1/admin/requests", ih.AdminRequestsHandler)
		mux.HandleFunc("/api/v1/admin/audit", ih.AdminAuditHandler)
		log.Info().Msg("identity layer enabled: /join onboarding + /api/v1/admin/* routes active")
	}
	if jobStore != nil {
		jh := restapi.NewJobsHandlers(jobStore, identityStore, aud, dispatchHub, scheduler, quotaDefaults, *apiKey)
		mux.HandleFunc("/api/v1/jobs", jh.JobsHandler)
		mux.HandleFunc("/api/v1/jobs/", jh.JobHandler)
		log.Info().Msg("job API enabled: /api/v1/jobs (submit/list/get/cancel)")
	}

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

// landingHTML is served at / so the public hostname doesn't show a bare
// "404 page not found". Unknown paths still fall through to http.NotFound.
const landingHTML = `<!doctype html>
<html><head><title>nmonit</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>body{font-family:system-ui;max-width:560px;margin:5rem auto;padding:0 1rem;color:#1c2733}
a{color:#2d6cdf}code{background:#f2f4f7;padding:.1rem .35rem;border-radius:4px}</style></head>
<body>
<h1>nmonit</h1>
<p>Distributed compute monitoring and job submission.</p>
<ul>
<li><a href="/health">/health</a> — cluster status (JSON)</li>
<li><a href="/join">/join</a> — request access to contribute or submit jobs</li>
<li><code>/api/v1/jobs</code> — job submission API (owner key or consumer token required)</li>
</ul>
</body></html>`
