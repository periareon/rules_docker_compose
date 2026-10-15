package images

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// PortMapping is a container port a service publishes, as declared in the
// compose file. The host port it ends up on is decided by the daemon at test
// time, so only the declared value is recorded.
type PortMapping struct {
	Service  string `json:"service"`
	Target   int    `json:"target"`
	Protocol string `json:"protocol"`
	// Published is the host port the compose file asked for, if any. Tests
	// never depend on it; it is kept for messages.
	Published string `json:"published,omitempty"`
}

// EnvPrefix is shared by the variables that tell a test where its services
// are reachable.
const EnvPrefix = "RULES_DOCKER_COMPOSE_"

// PortEnvName returns the environment variable carrying the host port for a
// service's container port, e.g. RULES_DOCKER_COMPOSE_PORT_NGINX_80.
func PortEnvName(service string, target int) string {
	return EnvPrefix + "PORT_" + envSegment(service) + "_" + fmt.Sprint(target)
}

// HostEnvName returns the environment variable carrying `host:port` for a
// service's container port, e.g. RULES_DOCKER_COMPOSE_HOST_NGINX_80.
func HostEnvName(service string, target int) string {
	return EnvPrefix + "HOST_" + envSegment(service) + "_" + fmt.Sprint(target)
}

func envSegment(name string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(name) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

// PublishedPort returns the address a published container port is reachable
// at from the host, from the publishers compose reports for the container.
func PublishedPort(publishers []Publisher, target int, protocol string) (host, port string, err error) {
	if protocol == "" {
		protocol = "tcp"
	}
	var matches []Publisher
	for _, p := range publishers {
		if p.TargetPort == target && p.Protocol == protocol && p.PublishedPort != 0 {
			matches = append(matches, p)
		}
	}
	if len(matches) == 0 {
		return "", "", fmt.Errorf("container port %d/%s is not published", target, protocol)
	}
	// Prefer an IPv4 binding; a wildcard address is reachable as loopback.
	chosen := matches[0]
	for _, p := range matches {
		if !strings.Contains(p.URL, ":") {
			chosen = p
			break
		}
	}
	host = chosen.URL
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return host, fmt.Sprint(chosen.PublishedPort), nil
}

// ExpandRefs replaces `${NAME}` and `$NAME` references in s with values from
// env, then from the process environment. Unknown names expand to the empty
// string, as a shell would. `$$` yields a literal `$`.
func ExpandRefs(s string, env map[string]string) string {
	return os.Expand(s, func(name string) string {
		if name == "$" {
			return "$"
		}
		if v, ok := env[name]; ok {
			return v
		}
		return os.Getenv(name)
	})
}

// FormatEnv renders an environment map as sorted KEY=VALUE pairs.
func FormatEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}
