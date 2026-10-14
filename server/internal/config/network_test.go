package config

import (
	"context"
	"strings"
	"testing"
)

func TestNetworkSTUNServers(t *testing.T) {
	for _, servers := range [][]string{nil, {"stun.example.com:3478", "8.8.8.8:65535", "[2606:4700:4700::1111]:1"}} {
		if err := (Network{STUNServers: servers}).Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, endpoint := range []string{"", "stun.example.com", "https://stun.example.com:3478", "stun.example.com/path:3478", " stun.example.com:3478", "stun.example.com:0", "stun.example.com:65536", "stun.example.com:03478", "stun.example.com:+3478", "127.0.0.1:3478", "10.0.0.1:3478", "169.254.0.1:3478", "0.0.0.0:3478", "224.0.0.1:3478", "[::1]:3478", "[::]:3478", "[ff02::1]:3478", "[fe80::1]:3478", "[fc00::1]:3478", "[::ffff:127.0.0.1]:3478", "[2606:4700::1%eth0]:3478", "bad_.example:3478", "-bad.example:3478"} {
		t.Run(endpoint, func(t *testing.T) {
			if err := (Network{STUNServers: []string{endpoint}}).Validate(); err == nil {
				t.Fatal("accepted invalid endpoint")
			}
		})
	}
	for _, servers := range [][]string{{"STUN.example.com:3478", "stun.example.com:3478"}, {"8.8.8.8:3478", "[::ffff:8.8.8.8]:3478"}, strings.Split(strings.Repeat("stun.example.com:3478,", 8)+"other.example.com:3478", ",")} {
		if err := (Network{STUNServers: servers}).Validate(); err == nil {
			t.Fatal("accepted duplicate or excess endpoints")
		}
	}
}

func TestLoadNetworkSTUNServers(t *testing.T) {
	for _, value := range []string{"", "stun.example.com:3478,8.8.8.8:3478", "stun.example.com:3478,", " stun.example.com:3478"} {
		cfg, err := Load(context.Background(), LoadOptions{LookupEnv: func(name string) (string, bool) { return value, name == "PAPERBOAT_NETWORK_STUN_SERVERS" }})
		if err != nil {
			t.Fatal(err)
		}
		valid := value == "" || value == "stun.example.com:3478,8.8.8.8:3478"
		if (cfg.Network.Validate() == nil) != valid {
			t.Fatalf("unexpected validation for %q", value)
		}
	}
}
