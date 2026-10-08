// Command nmonitctl is the cluster operator's management CLI for the nmonit
// identity layer. It talks to the control-plane REST API using the owner API
// key (X-API-Key) and manages users, per-node tokens, invites, join requests,
// and the audit trail.
//
// Examples:
//
//	nmonitctl --api-url https://nmonit.jerithai.com --api-key "$KEY" user add bob@example.com --role provider
//	nmonitctl token issue bob@example.com --label bob-laptop
//	nmonitctl token revoke 956835f2-07d9-4b98-b089-cb1e429b569a
//	nmonitctl invite create --role provider --max-uses 5
//	nmonitctl requests --status pending
//	nmonitctl approve <request-id>
//	nmonitctl audit tail --n 20
//
// Credentials can also come from env: NMONIT_API_URL and NMONIT_API_KEY.
package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func (c *client) do(method, path string, body any) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, strings.TrimSuffix(c.baseURL, "/")+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return data, resp.StatusCode, err
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "nmonitctl: "+format+"\n", args...)
	os.Exit(1)
}

func printJSON(data []byte) {
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, data, "", "  "); err != nil {
		fmt.Println(string(data))
		return
	}
	fmt.Println(pretty.String())
}

func tlsTransport(caFile string, insecure bool) http.RoundTripper {
	tlsCfg := &tls.Config{InsecureSkipVerify: insecure} //nolint:gosec // explicit dev flag
	if caFile != "" {
		if pem, err := os.ReadFile(caFile); err == nil {
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(pem)
			tlsCfg.RootCAs = pool
		}
	}
	return &http.Transport{TLSClientConfig: tlsCfg}
}

// flagsFirst moves flag arguments ahead of positional arguments so that
// `user add <email> --role consumer` behaves like `user add --role consumer
// <email>`: Go's flag package stops parsing at the first non-flag argument,
// silently ignoring flags that follow positionals. It assumes every flag
// takes a value (true for all nmonitctl subcommand flags).
func flagsFirst(args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") && i+1 < len(args) {
			flags = append(flags, args[i], args[i+1])
			i++
			continue
		}
		pos = append(pos, args[i])
	}
	return append(flags, pos...)
}

// jobCmd implements the owner-facing job subcommands (Phase 2).
func jobCmd(c *client, args []string) {
	if len(args) == 0 {
		fatalf("job: missing subcommand (submit|list|get|cancel)")
	}
	switch args[0] {
	case "submit":
		fs := flag.NewFlagSet("job submit", flag.ExitOnError)
		name := fs.String("name", "", "job name")
		image := fs.String("image", "", "OCI container image")
		cpu := fs.Float64("cpu", 1, "cpu cores")
		memGB := fs.Float64("mem-gb", 1, "memory GiB")
		fs.Parse(flagsFirst(args[1:]))
		if *name == "" || *image == "" {
			fatalf("job submit: --name and --image are required")
		}
		body := map[string]any{
			"name": *name, "image": *image,
			"cpu_cores": *cpu, "mem_bytes": uint64(*memGB * (1 << 30)),
		}
		data, code, err := c.do("POST", "/api/v1/jobs", body)
		exit(err, code)
		printJSON(data)
	case "list":
		data, code, err := c.do("GET", "/api/v1/jobs", nil)
		exit(err, code)
		printJSON(data)
	case "get":
		if len(args) < 2 {
			fatalf("job get: missing job id")
		}
		data, code, err := c.do("GET", "/api/v1/jobs/"+args[1], nil)
		exit(err, code)
		printJSON(data)
	case "cancel":
		if len(args) < 2 {
			fatalf("job cancel: missing job id")
		}
		data, code, err := c.do("DELETE", "/api/v1/jobs/"+args[1], nil)
		exit(err, code)
		printJSON(data)
	default:
		fatalf("job: unknown subcommand %q", args[0])
	}
}

func main() {
	apiURL := flag.String("api-url", envOr("NMONIT_API_URL", "http://127.0.0.1:8080"), "control-plane REST base URL")
	apiKey := flag.String("api-key", os.Getenv("NMONIT_API_KEY"), "owner API key")
	caFile := flag.String("ca", os.Getenv("NMONIT_CA_FILE"), "CA cert PEM for self-signed control planes")
	insecure := flag.Bool("insecure", false, "skip TLS verification (dev only)")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, `nmonitctl — nmonit identity management

Usage:
  nmonitctl [flags] <command> <subcommand> [args]

Commands:
  user add <email> --role provider|consumer|owner [--name NAME]
  user list
  user disable <user-id>
  token list <email>
  token issue <email> [--label LABEL]     (prints the nmt_ secret ONCE)
  token revoke <token-id>
  invite create --role provider|consumer [--max-uses N] [--expiry-hours H]
  invite list
  requests [--status pending|approved|denied]
  approve <request-id>                    (creates user + first node token)
  deny <request-id>
  audit tail [--n N]
  job submit --name NAME --image IMAGE [--cpu N] [--mem-gb N]
  job list
  job get <job-id>
  job cancel <job-id>

  (job commands act as the owner via the API key; consumers submit their
   own jobs through the REST/gRPC API with their nmt_ token)

Flags:
  --api-url   control-plane base URL (default NMONIT_API_URL or http://127.0.0.1:8080)
  --api-key   owner API key (default NMONIT_API_KEY)`)
	}
	flag.Parse()

	if *apiKey == "" {
		fatalf("no API key: pass --api-key or set NMONIT_API_KEY")
	}
	c := &client{baseURL: *apiURL, apiKey: *apiKey,
		http: &http.Client{Timeout: 15 * time.Second, Transport: tlsTransport(*caFile, *insecure)}}

	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	switch args[0] {
	case "user":
		userCmd(c, args[1:])
	case "token":
		tokenCmd(c, args[1:])
	case "invite":
		inviteCmd(c, args[1:])
	case "requests":
		status := ""
		if len(args) > 2 && args[1] == "--status" {
			status = args[2]
		}
		data, code, err := c.do("GET", "/api/v1/admin/requests?status="+status, nil)
		exit(err, code)
		printJSON(data)
	case "approve", "deny":
		if len(args) < 2 {
			fatalf("usage: %s <request-id>", args[0])
		}
		data, code, err := c.do("POST", "/api/v1/admin/requests", map[string]any{
			"id": args[1], "approve": args[0] == "approve",
		})
		exit(err, code)
		printJSON(data)
	case "job":
		jobCmd(c, args[1:])
	case "audit":
		if len(args) > 1 && args[1] == "tail" {
			n := "50"
			if len(args) > 3 && args[2] == "--n" {
				n = args[3]
			}
			data, code, err := c.do("GET", "/api/v1/admin/audit?n="+n, nil)
			exit(err, code)
			printJSON(data)
			return
		}
		fatalf("unknown audit subcommand %q (want: tail)", args[1])
	default:
		fatalf("unknown command %q — run nmonitctl -h", args[0])
	}
}

func userCmd(c *client, args []string) {
	if len(args) == 0 {
		fatalf("usage: user add|list|disable ...")
	}
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("user add", flag.ExitOnError)
		role := fs.String("role", "provider", "provider|consumer|owner")
		name := fs.String("name", "", "display name")
		_ = fs.Parse(flagsFirst(args[1:]))
		if fs.NArg() < 1 {
			fatalf("usage: user add <email> --role ...")
		}
		data, code, err := c.do("POST", "/api/v1/admin/users", map[string]any{
			"email": fs.Arg(0), "display_name": *name, "role": *role,
		})
		exit(err, code)
		printJSON(data)
	case "list":
		data, code, err := c.do("GET", "/api/v1/admin/users", nil)
		exit(err, code)
		printJSON(data)
	case "disable":
		if len(args) < 2 {
			fatalf("usage: user disable <user-id>")
		}
		data, code, err := c.do("POST", "/api/v1/admin/users/"+args[1]+"/status",
			map[string]any{"status": "disabled"})
		exit(err, code)
		printJSON(data)
	default:
		fatalf("unknown user subcommand %q", args[0])
	}
}

func tokenCmd(c *client, args []string) {
	if len(args) == 0 {
		fatalf("usage: token list|issue|revoke ...")
	}
	switch args[0] {
	case "list":
		if len(args) < 2 {
			fatalf("usage: token list <email>")
		}
		data, code, err := c.do("GET", "/api/v1/admin/tokens?user="+args[1], nil)
		exit(err, code)
		printJSON(data)
	case "issue":
		fs := flag.NewFlagSet("token issue", flag.ExitOnError)
		label := fs.String("label", "", "token label")
		_ = fs.Parse(flagsFirst(args[1:]))
		if fs.NArg() < 1 {
			fatalf("usage: token issue <email> --label ...")
		}
		data, code, err := c.do("POST", "/api/v1/admin/tokens?user="+fs.Arg(0),
			map[string]any{"label": *label})
		exit(err, code)
		printJSON(data)
	case "revoke":
		if len(args) < 2 {
			fatalf("usage: token revoke <token-id>")
		}
		data, code, err := c.do("POST", "/api/v1/admin/tokens/"+args[1]+"/revoke", map[string]any{})
		exit(err, code)
		printJSON(data)
	default:
		fatalf("unknown token subcommand %q", args[0])
	}
}

func inviteCmd(c *client, args []string) {
	if len(args) == 0 {
		fatalf("usage: invite create|list ...")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("invite create", flag.ExitOnError)
		role := fs.String("role", "provider", "provider|consumer")
		maxUses := fs.Int("max-uses", 1, "max uses")
		expiry := fs.Int("expiry-hours", 72, "expiry in hours")
		_ = fs.Parse(flagsFirst(args[1:]))
		data, code, err := c.do("POST", "/api/v1/admin/invites", map[string]any{
			"role": *role, "max_uses": *maxUses, "expiry_hours": *expiry,
		})
		exit(err, code)
		printJSON(data)
	case "list":
		data, code, err := c.do("GET", "/api/v1/admin/invites", nil)
		exit(err, code)
		printJSON(data)
	default:
		fatalf("unknown invite subcommand %q", args[0])
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func exit(err error, code int) {
	if err != nil {
		fatalf("%v", err)
	}
	if code >= 400 {
		os.Exit(1)
	}
}
