//go:build !shn_coreonly

package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// probeTimeout bounds one probe. It is past GET /health's slowest answer, a
// store ping bounded at 2 s (sdk/health DBPing), so a slow or unreachable
// database delays the answer without failing the probe; a container health
// check's own timeout should be past it too (ECS's default is 5 s).
const probeTimeout = 4 * time.Second

// probeMode answers `gateway -probe <url>`, the gateway's container health
// check: the published image is distroless, with no shell or HTTP client, so
// a runtime runs the binary itself in exec form against the gateway's own
// port, for example ["CMD", "/gateway", "-probe", "http://127.0.0.1:8080/health"].
// It reads no setting, file or network but the URL it is given, which names a
// loopback IP address itself (such as 127.0.0.1 or [::1], never a name to resolve). It
// reports whether it answered and the exit status: 0 when the URL answers 200,
// 1 on any other answer or a failed request, 2 for any other usage (no URL, more
// than one, or one that is not such a URL). It writes nothing on success and
// one line on failure, which never repeats the URL.
func probeMode(args []string, stderr io.Writer) (code int, answered bool) {
	if len(args) == 0 || args[0] != "-probe" {
		return 0, false
	}
	if len(args) != 2 {
		fmt.Fprintln(stderr, "gateway: usage: gateway -probe http://127.0.0.1:<port>/health")
		return 2, true
	}
	return probe(args[1], stderr), true
}

// probe reads target once, on the loopback only: a probe that could read
// another host would report another process's health as this one's.
func probe(target string, stderr io.Writer) int {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "http" || u.User != nil || !loopbackHost(u.Hostname()) || !validPort(u.Port()) {
		fmt.Fprintln(stderr, "gateway: -probe: the URL is not http on a loopback IP address with a valid port")
		return 2
	}
	c := &http.Client{
		Timeout: probeTimeout,
		// No proxy from the environment and no redirect: the answer is the
		// URL's own.
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := c.Get(u.String())
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			fmt.Fprintln(stderr, "gateway: -probe: the request timed out")
		} else {
			fmt.Fprintln(stderr, "gateway: -probe: the request failed")
		}
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "gateway: -probe: answered %d\n", resp.StatusCode)
		return 1
	}
	return 0
}

// loopbackHost reports whether host is a loopback address written as one. A
// name, localhost included, is refused: resolving it reads the host's files
// or the network, and may not name the loopback.
func loopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validPort reports whether port, as a URL carries it, is absent (http's 80)
// or a number from 1 to 65535.
func validPort(port string) bool {
	if port == "" {
		return true
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}
