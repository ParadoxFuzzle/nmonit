/// Control plane main entry point.
///
/// Starts the gRPC server, REST API gateway, scheduler, and
/// Raft consensus cluster.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func main() {
	// CLI flags
	var (
		grpcAddr   = flag.String("grpc-addr", ":9000", "gRPC listen address")
		httpAddr   = flag.String("http-addr", ":8080", "REST API listen address")
		raftAddr   = flag.String("raft-addr", ":9001", "Raft consensus listen address")
		raftDir    = flag.String("raft-dir", "/var/lib/compute-control-plane/raft", "Raft persistent storage")
		nodeID     = flag.String("node-id", "", "Unique node ID for Raft (defaults to hostname)")
		peers      = flag.String("peers", "", "Comma-separated list of peer addresses (for joining existing cluster)")
		logLevel   = flag.String("log-level", "info", "Log level (debug, info, warn, error)")
		bootstrap  = flag.Bool("bootstrap", false, "Bootstrap a new Raft cluster (first node only)")
	)
	flag.Parse()

	// Setup structured logging
	level, err := zerolog.ParseLevel(*logLevel)
	if err != nil {
		level = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(level)
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

	hostname, _ := os.Hostname()
	if *nodeID == "" {
		*nodeID = hostname
	}

	log.Info().
		Str("node_id", *nodeID).
		Str("grpc_addr", *grpcAddr).
		Str("http_addr", *httpAddr).
		Str("raft_addr", *raftAddr).
		Bool("bootstrap", *bootstrap).
		Msg("compute-control-plane starting")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// TODO: Initialize Raft cluster (Phase 1)
	// TODO: Start gRPC server (Phase 1)
	// TODO: Start REST API gateway (Phase 1)
	// TODO: Start scheduler loop (Phase 1)
	// TODO: Start health monitor (Phase 1)

	_ = raftDir
	_ = peers
	_ = ctx

	log.Info().Msg("compute-control-plane initialized and ready")

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh

	log.Info().Str("signal", sig.String()).Msg("shutting down")

	fmt.Println("Shutdown complete")
}
