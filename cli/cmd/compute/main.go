// Command compute is the nmonit control-plane CLI.
//
// It talks to the REST API only, so it works with both the plaintext
// development server and the TLS/mTLS production server.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

const (
	defaultServer    = "http://localhost:8080"
	maxResponseBytes = 8 << 20
)

// version can be overridden at build time:
//
//	go build -ldflags "-X main.version=1.2.3"
var version = "0.1.0"

type options struct {
	server  string
	apiKey  string
	caCert  string
	timeout time.Duration
	jsonOut bool
}

type node struct {
	NodeID      string           `json:"node_id"`
	Hostname    string           `json:"hostname"`
	Address     string           `json:"address"`
	CPU         cpuResources     `json:"cpu"`
	Memory      memoryResources  `json:"memory"`
	GPUs        []gpuResources   `json:"gpus"`
	LastBeat    string           `json:"last_beat"`
	Utilization *nodeUtilization `json:"utilization,omitempty"`
}

type cpuResources struct {
	PhysicalCores int    `json:"physical_cores"`
	LogicalCores  int    `json:"logical_cores"`
	Architecture  string `json:"architecture"`
	Model         string `json:"model"`
}

type memoryResources struct {
	TotalGB float64 `json:"total_gb"`
}

type gpuResources struct {
	Index  int     `json:"index"`
	Vendor string  `json:"vendor"`
	Model  string  `json:"model"`
	VRAMGB float64 `json:"vram_gb"`
}

type nodeUtilization struct {
	Timestamp string             `json:"timestamp"`
	CPU       *cpuUtilization    `json:"cpu"`
	Memory    *memoryUtilization `json:"memory"`
	GPUs      []gpuUtilization   `json:"gpus"`
}

type cpuUtilization struct {
	OverallPercent float64   `json:"overall_percent"`
	PerCorePercent []float64 `json:"per_core_percent"`
	Load1          float64   `json:"load_1m"`
	Load5          float64   `json:"load_5m"`
	Load15         float64   `json:"load_15m"`
}

type memoryUtilization struct {
	UsedGB      float64 `json:"used_gb"`
	AvailableGB float64 `json:"available_gb"`
	TotalGB     float64 `json:"total_gb"`
}

type gpuUtilization struct {
	Index          int     `json:"index"`
	GPUPercent     float64 `json:"gpu_percent"`
	MemoryPercent  float64 `json:"memory_percent"`
	VRAMUsedGB     float64 `json:"vram_used_gb"`
	VRAMTotalGB    float64 `json:"vram_total_gb"`
	TemperatureC   int     `json:"temperature_c"`
	PowerDrawW     int64   `json:"power_draw_w"`
	PowerLimitW    int64   `json:"power_limit_w"`
	ClockMHz       int64   `json:"clock_mhz"`
	MemoryClockMHz int64   `json:"memory_clock_mhz"`
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "compute:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	opts := &options{}
	root := &cobra.Command{
		Use:           "compute",
		Short:         "CLI for the nmonit distributed compute fabric",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVar(&opts.server, "server", envOr("NMONIT_SERVER", defaultServer), "control-plane REST base URL")
	root.PersistentFlags().StringVar(&opts.apiKey, "api-key", os.Getenv("NMONIT_API_KEY"), "REST API key (or NMONIT_API_KEY)")
	root.PersistentFlags().StringVar(&opts.caCert, "ca-cert", os.Getenv("NMONIT_CA_CERT"), "CA certificate for HTTPS (or NMONIT_CA_CERT)")
	root.PersistentFlags().DurationVar(&opts.timeout, "timeout", 10*time.Second, "HTTP request timeout")
	root.PersistentFlags().BoolVar(&opts.jsonOut, "json", false, "Print machine-readable JSON")

	root.AddCommand(newHealthCmd(opts), newStatusCmd(opts), newVersionCmd())
	return root
}

func newHealthCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "Check control-plane health",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var response map[string]string
			if err := fetchJSON(opts, "/health", &response); err != nil {
				return err
			}
			if opts.jsonOut {
				return writeJSON(cmd.OutOrStdout(), response)
			}
			status := response["status"]
			if status == "" {
				status = "ok"
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), status)
			return err
		},
	}
}

func newStatusCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:     "status",
		Aliases: []string{"nodes"},
		Short:   "Show registered nodes and their live utilization",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var nodes []node
			if err := fetchJSON(opts, "/api/v1/nodes", &nodes); err != nil {
				return err
			}
			if opts.jsonOut {
				return writeJSON(cmd.OutOrStdout(), nodes)
			}
			writeNodeTable(cmd.OutOrStdout(), nodes)
			return nil
		},
	}
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "compute %s\n", version)
		},
	}
}

func fetchJSON(opts *options, path string, out any) error {
	base := strings.TrimRight(opts.server, "/")
	if base == "" {
		return fmt.Errorf("server URL must not be empty")
	}

	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if opts.apiKey != "" {
		req.Header.Set("X-API-Key", opts.apiKey)
	}

	client, err := newHTTPClient(opts)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		var apiErr struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &apiErr) == nil && apiErr.Error != "" {
			return fmt.Errorf("%s: %s", resp.Status, apiErr.Error)
		}
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("%s: %s", resp.Status, message)
	}

	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func newHTTPClient(opts *options) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if opts.caCert != "" {
		pem, err := os.ReadFile(opts.caCert)
		if err != nil {
			return nil, fmt.Errorf("read CA certificate: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("parse CA certificate %s: no valid PEM found", opts.caCert)
		}
		transport.TLSClientConfig = &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS12,
		}
	}
	return &http.Client{Transport: transport, Timeout: opts.timeout}, nil
}

func writeJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func writeNodeTable(w io.Writer, nodes []node) {
	if len(nodes) == 0 {
		fmt.Fprintln(w, "No nodes registered.")
		return
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeID < nodes[j].NodeID })

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tHOST\tSTATE\tCPU\tMEMORY\tGPU\tLAST BEAT")
	for _, n := range nodes {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			n.NodeID,
			n.Hostname,
			nodeState(n.LastBeat),
			formatCPU(n),
			formatMemory(n),
			formatGPUs(n),
			n.LastBeat,
		)
	}
	_ = tw.Flush()
}

func nodeState(lastBeat string) string {
	t, err := time.Parse(time.RFC3339, lastBeat)
	if err != nil {
		return "unknown"
	}
	if time.Since(t) > 30*time.Second {
		return "stale"
	}
	return "online"
}

func formatCPU(n node) string {
	cores := fmt.Sprintf("%d/%d", n.CPU.PhysicalCores, n.CPU.LogicalCores)
	if n.Utilization != nil && n.Utilization.CPU != nil {
		return fmt.Sprintf("%.1f%% (%s)", n.Utilization.CPU.OverallPercent, cores)
	}
	return cores
}

func formatMemory(n node) string {
	if n.Utilization != nil && n.Utilization.Memory != nil {
		total := n.Utilization.Memory.TotalGB
		if total == 0 {
			total = n.Memory.TotalGB
		}
		return fmt.Sprintf("%.1f/%.1f GB", n.Utilization.Memory.UsedGB, total)
	}
	return fmt.Sprintf("%.1f GB", n.Memory.TotalGB)
}

func formatGPUs(n node) string {
	if len(n.GPUs) == 0 {
		return "-"
	}
	if n.Utilization == nil || len(n.Utilization.GPUs) == 0 {
		return fmt.Sprintf("%d", len(n.GPUs))
	}

	parts := make([]string, 0, len(n.Utilization.GPUs))
	for _, g := range n.Utilization.GPUs {
		parts = append(parts, fmt.Sprintf("%d: %.0f%% %.1f/%.1f GB",
			g.Index, g.GPUPercent, g.VRAMUsedGB, g.VRAMTotalGB))
	}
	return strings.Join(parts, "; ")
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
