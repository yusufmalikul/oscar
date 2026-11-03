package engine

import (
	"context"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Local ports held by these programs usually forward to another machine.
var forwarders = map[string]bool{"ssh": true, "tsh": true, "kubectl": true, "oc": true, "gcloud": true,
	"cloudflared": true, "session-manager-plugin": true, "socat": true, "sshuttle": true, "teleport": true}

// Forwarders that only reach local VMs (Colima/Lima docker port mapping) are not remote.
var localVM = regexp.MustCompile(`/\.colima/|/\.lima/|/_lima/|\blimactl\b`)

type portCache struct {
	mu sync.Mutex
	at map[string]time.Time
	v  map[string]bool
}

var forwardedCache = portCache{at: map[string]time.Time{}, v: map[string]bool{}}

// portForwarded reports whether the program listening on 127.0.0.1:port is a tunnel to
// another machine. When the check itself fails, the port is treated as forwarded (safe side).
func portForwarded(port string) bool {
	forwardedCache.mu.Lock()
	if t, ok := forwardedCache.at[port]; ok && time.Since(t) < 10*time.Second {
		v := forwardedCache.v[port]
		forwardedCache.mu.Unlock()
		return v
	}
	forwardedCache.mu.Unlock()

	v := checkForwarded(port)
	forwardedCache.mu.Lock()
	forwardedCache.at[port], forwardedCache.v[port] = time.Now(), v
	forwardedCache.mu.Unlock()
	return v
}

func checkForwarded(port string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", "-nP", "-iTCP:"+port, "-sTCP:LISTEN", "-Fpc").Output()
	if err != nil {
		// lsof exits 1 when nothing listens: nothing to reach, so not a tunnel.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 && len(out) == 0 {
			return false
		}
		return true
	}
	var pid string
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "p"):
			pid = line[1:]
		case strings.HasPrefix(line, "c"):
			if !forwarders[line[1:]] {
				continue
			}
			args, _ := exec.CommandContext(ctx, "ps", "-o", "args=", "-p", pid).Output()
			if !localVM.Match(args) {
				return true
			}
		}
	}
	return false
}
