package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/cloudflare/cloudflared/cmd/cloudflared/cliutil"
	"github.com/cloudflare/cloudflared/cmd/cloudflared/tunnel"
)

var tunnelHostnameRe = regexp.MustCompile(`https://([a-z0-9-]+\.trycloudflare\.com)`)

// ---------------------------------------------------------------------
// Env-var driven config: lets both ends bypass Cloudflare entirely and
// talk over plain localhost, for tests that need to run with no network
// access and a predictable port. Not meant for real transfers - the
// tunnel is what makes this work across two different machines/NATs.
// ---------------------------------------------------------------------

const (
	envLocalMode = "LEFI_LOCAL" // any non-empty value: skip the tunnel, use plain HTTP on localhost
	envPort      = "LEFI_PORT"  // bind this exact local port instead of a random one
)

func localModeEnabled() bool {
	return os.Getenv(envLocalMode) != ""
}

// cloudflaredEnvVars is every environment variable name cloudflared's own
// flag definitions declare as an EnvVars source, across its entire
// command tree - extracted directly from cmd/cloudflared source (grep -rhoE
// 'EnvVars:\s*\[\]string\{[^}]*\}'), not hand-guessed. Most are TUNNEL_-
// prefixed; the rest belong to the access/tail/ssh subcommands we never
// invoke, kept here anyway for defense in depth against future cloudflared
// changes.
var cloudflaredEnvVars = []string{
	"ACCESS_CLIENT_ID", "BUCKET_ID", "DIAL_EDGE_TIMEOUT", "HOST_KEY_PATH",
	"LOCAL_SSH_PORT", "NO_AUTOUPDATE", "NO_TLS_VERIFY", "REGION_ID",
	"S3_URL", "SECRET_ID", "SESSION_TOKEN_ID", "SSH_IDLE_TIMEOUT",
	"SSH_MAX_TIMEOUT", "STDIN_CONTROL",
	"TUNNEL_API_CA_KEY", "TUNNEL_API_EMAIL", "TUNNEL_API_KEY", "TUNNEL_API_URL",
	"TUNNEL_BASTION", "TUNNEL_CACERT", "TUNNEL_CLEANUP_CONNECTOR",
	"TUNNEL_COMPRESSION_LEVEL", "TUNNEL_CREATE_SECRET", "TUNNEL_CRED_CONTENTS",
	"TUNNEL_CRED_FILE", "TUNNEL_DISABLE_QUIC_PMTU", "TUNNEL_DNS_RESOLVER_ADDRS",
	"TUNNEL_EDGE", "TUNNEL_EDGE_BIND_ADDRESS", "TUNNEL_EDGE_IP_VERSION",
	"TUNNEL_FORCE_PROVISIONING_DNS", "TUNNEL_GRACE_PERIOD", "TUNNEL_HELLO_WORLD",
	"TUNNEL_HOSTNAME", "TUNNEL_HTTP_HOST_HEADER", "TUNNEL_ICMPV4_SRC",
	"TUNNEL_ICMPV6_SRC", "TUNNEL_ID", "TUNNEL_INFO_INVERT_SORT",
	"TUNNEL_INFO_SORT_BY", "TUNNEL_INGRESS_VALIDATE_JSON", "TUNNEL_LB_POOL",
	"TUNNEL_LIST_INVERT_SORT", "TUNNEL_LIST_SORT_BY", "TUNNEL_LOGDIRECTORY",
	"TUNNEL_LOGFILE", "TUNNEL_LOGLEVEL", "TUNNEL_LOG_OUTPUT",
	"TUNNEL_MANAGEMENT_CONNECTOR", "TUNNEL_MANAGEMENT_DIAGNOSTICS",
	"TUNNEL_MANAGEMENT_FILTER_EVENTS", "TUNNEL_MANAGEMENT_FILTER_LEVEL",
	"TUNNEL_MANAGEMENT_FILTER_SAMPLE", "TUNNEL_MANAGEMENT_HOSTNAME",
	"TUNNEL_MANAGEMENT_OUTPUT", "TUNNEL_MANAGEMENT_TOKEN", "TUNNEL_MAX_ACTIVE_FLOWS",
	"TUNNEL_MAX_FETCH_SIZE", "TUNNEL_METRICS", "TUNNEL_METRICS_UPDATE_FREQ",
	"TUNNEL_NAME", "TUNNEL_NO_CHUNKED_ENCODING", "TUNNEL_NO_PRECHECKS",
	"TUNNEL_ORIGIN_CA_POOL", "TUNNEL_ORIGIN_CERT", "TUNNEL_ORIGIN_ENABLE_HTTP2",
	"TUNNEL_ORIGIN_SERVER_NAME", "TUNNEL_PIDFILE", "TUNNEL_POST_QUANTUM",
	"TUNNEL_PROTO_LOGLEVEL", "TUNNEL_PROXY_ADDRESS", "TUNNEL_PROXY_PORT",
	"TUNNEL_QUIC_CONN_LEVEL_FLOW_CONTROL_LIMIT", "TUNNEL_QUIC_STREAM_LEVEL_FLOW_CONTROL_LIMIT",
	"TUNNEL_REGION", "TUNNEL_RETRIES", "TUNNEL_RUN_FORCE_OVERWRITE",
	"TUNNEL_SERVICE_DESTINATION", "TUNNEL_SERVICE_HOSTNAME", "TUNNEL_SERVICE_OP_IP",
	"TUNNEL_SERVICE_TOKEN_ID", "TUNNEL_SERVICE_TOKEN_SECRET", "TUNNEL_SERVICE_URL",
	"TUNNEL_SOCKS", "TUNNEL_SSH_SERVER", "TUNNEL_STREAM_WRITE_TIMEOUT",
	"TUNNEL_TAG", "TUNNEL_TOKEN", "TUNNEL_TOKEN_FILE", "TUNNEL_TRACE_OUTPUT",
	"TUNNEL_TRANSPORT_LOGLEVEL", "TUNNEL_TRANSPORT_PROTOCOL", "TUNNEL_UNIX_SOCKET",
	"TUNNEL_URL", "TUNNEL_USE_RECONNECT_TOKEN",
}

// clearCloudflaredEnv unsets every environment variable cloudflared
// recognizes, so a real cloudflared setup on this machine (from the
// person's own separate use of it) can't leak in and influence what's
// meant to be an anonymous, account-free quick tunnel. Only called right
// before bringing up a real tunnel - never in LEFI_LOCAL mode, which
// doesn't touch cloudflared's code at all.
func clearCloudflaredEnv() {
	for _, name := range cloudflaredEnvVars {
		os.Unsetenv(name)
	}
}

// targetURL builds the address a client should hit, given the single
// channel argument. In local mode "target" is a plain host:port and the
// URL is unencrypted http; normally it's a trycloudflare.com channel name
// over https.
func targetURL(target string) string {
	if localModeEnabled() {
		return fmt.Sprintf("http://%s/", target)
	}
	return fmt.Sprintf("https://%s.trycloudflare.com/", target)
}

// tunnelSession represents a live local HTTP server plus (unless in local
// mode) a Cloudflare Quick Tunnel exposing it.
type tunnelSession struct {
	channel    string
	url        string
	localPort  int
	srv        *http.Server
	cancel     context.CancelFunc
	tunnelErrC chan error
}

// startTunnel binds a local listener, serves handler on it, and brings up
// either a real Cloudflare Quick Tunnel or (LEFI_LOCAL) a plain localhost
// endpoint. Blocks until a channel/URL is ready (or setup fails).
func startTunnel(ui *os.File, handler http.Handler) (*tunnelSession, error) {
	bindPort := 0
	if p := os.Getenv(envPort); p != "" {
		v, perr := strconv.Atoi(p)
		if perr != nil || v < 0 || v > 65535 {
			fmt.Fprintf(ui, "warning: invalid %s=%q, ignoring (using a random port)\n", envPort, p)
		} else {
			bindPort = v
		}
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", bindPort))
	if err != nil {
		return nil, fmt.Errorf("opening local listener: %w", err)
	}
	localPort := ln.Addr().(*net.TCPAddr).Port

	srv := &http.Server{Handler: handler}
	go srv.Serve(ln)

	sess := &tunnelSession{localPort: localPort, srv: srv}

	if localModeEnabled() {
		sess.tunnelErrC = make(chan error)
		close(sess.tunnelErrC) // nothing to wait for; reading it returns immediately
		sess.cancel = func() {}
		sess.channel = fmt.Sprintf("127.0.0.1:%d", localPort)
		sess.url = "http://" + sess.channel + "/"
		fmt.Fprintf(ui, "Local mode active (%s set) - no Cloudflare tunnel, listening on %s.\n", envLocalMode, sess.channel)
		fmt.Fprintln(ui)
		return sess, nil
	}

	logPath, err := os.CreateTemp("", "le-fi-*.log")
	if err != nil {
		return nil, fmt.Errorf("creating temp logfile: %w", err)
	}
	logPath.Close()

	// Isolate from any existing cloudflared setup on this machine. We're
	// running cloudflared's own code in-process, sharing this same OS
	// environment - without this, a real ~/.cloudflared/config.yml or
	// TUNNEL_* env vars left over from the person's own cloudflared use
	// would get silently picked up and influence (or outright hijack)
	// what's supposed to be an anonymous, account-free quick tunnel.
	clearCloudflaredEnv()

	bInfo := cliutil.GetBuildInfo("", "le-fi")
	graceShutdownC := make(chan struct{})
	tunnel.Init(bInfo, graceShutdownC)

	app := &cli.App{
		Name:     "cloudflared",
		Commands: tunnel.Commands(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	sess.cancel = func() {
		cancel()
		os.Remove(logPath.Name())
	}

	tunnelErrC := make(chan error, 1)
	sess.tunnelErrC = tunnelErrC
	go func() {
		tunnelErrC <- app.RunContext(ctx, []string{
			"cloudflared", "tunnel",
			"--url", fmt.Sprintf("http://127.0.0.1:%d", localPort),
			"--no-autoupdate",
			"--logfile", logPath.Name(),
			// cloudflared's own docs warn the metrics listener can bind
			// to all interfaces "when running from a virtual
			// environment" - pin it to localhost explicitly rather
			// than trust that detection.
			"--metrics", "127.0.0.1:0",
			// Defaults to true upstream; disables the expanded
			// /debug/pprof, /metrics, etc. routes reachable through
			// the tunnel's management channel.
			"--management-diagnostics=false",
			// Force no config file and no account certificate, rather
			// than letting cloudflared fall back to auto-discovering
			// ~/.cloudflared/config.yml or cert.pem. Quick tunnels get
			// ephemeral, account-free credentials straight from the
			// trycloudflare.com API and need neither - explicit empty
			// values here override that auto-discovery outright.
			"--config", "",
			"--origincert", "",
		})
	}()

	hostname, err := waitForTunnelHostname(logPath.Name(), tunnelErrC, 20*time.Second)
	if err != nil {
		sess.cancel()
		return nil, err
	}
	sess.channel = hostname[:len(hostname)-len(".trycloudflare.com")]
	sess.url = "https://" + hostname + "/"
	fmt.Fprintln(ui, "Tunnel is up (no account, no port forwarding - works through any NAT).")
	fmt.Fprintln(ui)
	return sess, nil
}

// shutdown tears the session down: gives any in-flight HTTP response a
// moment to flush, cancels the tunnel (a no-op in local mode), closes the
// local server, and waits (briefly) for the tunnel goroutine to exit.
func (s *tunnelSession) shutdown() {
	time.Sleep(300 * time.Millisecond)
	s.cancel()
	_ = s.srv.Close()
	select {
	case <-s.tunnelErrC:
	case <-time.After(3 * time.Second):
	}
}

func waitForTunnelHostname(logPath string, tunnelErrC chan error, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-tunnelErrC:
			return "", fmt.Errorf("cloudflared tunnel exited early: %w", err)
		default:
		}
		data, err := os.ReadFile(logPath)
		if err == nil {
			if m := tunnelHostnameRe.FindStringSubmatch(string(data)); m != nil {
				return m[1], nil
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return "", fmt.Errorf("timed out waiting for a tunnel hostname")
}
