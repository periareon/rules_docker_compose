package images

import (
	"reflect"
	"testing"
)

func TestEnvNames(t *testing.T) {
	if got := PortEnvName("nginx", 80); got != "RULES_DOCKER_COMPOSE_PORT_NGINX_80" {
		t.Errorf("PortEnvName = %q", got)
	}
	if got := HostEnvName("my-db.primary", 5432); got != "RULES_DOCKER_COMPOSE_HOST_MY_DB_PRIMARY_5432" {
		t.Errorf("HostEnvName = %q", got)
	}
}

func TestPublishedPort(t *testing.T) {
	ports := []Publisher{
		{URL: "::", TargetPort: 80, PublishedPort: 49153, Protocol: "tcp"},
		{URL: "0.0.0.0", TargetPort: 80, PublishedPort: 49153, Protocol: "tcp"},
		{URL: "", TargetPort: 443, PublishedPort: 0, Protocol: "tcp"},
		{URL: "127.0.0.1", TargetPort: 53, PublishedPort: 5353, Protocol: "udp"},
	}
	host, port, err := PublishedPort(ports, 80, "tcp")
	if err != nil || host != "127.0.0.1" || port != "49153" {
		t.Errorf("80/tcp: %s %s %v", host, port, err)
	}
	host, port, err = PublishedPort(ports, 53, "udp")
	if err != nil || host != "127.0.0.1" || port != "5353" {
		t.Errorf("53/udp: %s %s %v", host, port, err)
	}
	if _, _, err := PublishedPort(ports, 443, "tcp"); err == nil {
		t.Errorf("443/tcp should be reported as unpublished")
	}
	if _, _, err := PublishedPort(ports, 8080, ""); err == nil {
		t.Errorf("8080/tcp should be reported as unpublished")
	}
}

func TestExpandRefs(t *testing.T) {
	env := map[string]string{"RULES_DOCKER_COMPOSE_HOST_NGINX_80": "127.0.0.1:49153"}
	cases := map[string]string{
		"http://${RULES_DOCKER_COMPOSE_HOST_NGINX_80}/": "http://127.0.0.1:49153/",
		"$RULES_DOCKER_COMPOSE_HOST_NGINX_80":           "127.0.0.1:49153",
		"cost: $$5":                                     "cost: $5",
		"${RULES_DOCKER_COMPOSE_NOPE}":                  "",
		"plain":                                         "plain",
	}
	for in, want := range cases {
		if got := ExpandRefs(in, env); got != want {
			t.Errorf("ExpandRefs(%q) = %q, want %q", in, got, want)
		}
	}
	if got := FormatEnv(map[string]string{"B": "2", "A": "1"}); !reflect.DeepEqual(got, []string{"A=1", "B=2"}) {
		t.Errorf("FormatEnv = %v", got)
	}
}
