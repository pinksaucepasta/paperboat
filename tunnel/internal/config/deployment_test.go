package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDeploymentExampleLoads(t *testing.T) {
	if _, err := LoadDeployment(filepath.Join("..", "..", "deploy", "deployment.example.json")); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDeploymentStrictProfile(t *testing.T) {
	path := writeDeployment(t, strings.TrimSuffix(validDeploymentJSON(), "}")+`,"public_https_cert_file":"/cert.pem","public_https_key_file":"/key.pem"}`)
	deployment, err := LoadDeployment(path)
	if err != nil {
		t.Fatal(err)
	}
	if deployment.PublicHTTPSCertFile != "/cert.pem" || deployment.PublicHTTPSKeyFile != "/key.pem" {
		t.Fatal("public HTTPS certificate paths were not retained")
	}
	if deployment.CarrierTCPListenAddress != "0.0.0.0:27443" || deployment.CarrierQUICListenAddress != "0.0.0.0:27444" || deployment.PreviewBaseDomain != "preview.example.test" || deployment.TunnelBaseDomain != "tunnels.example.test" || deployment.RuntimeBaseDomain != "runtime.example.test" || deployment.NodeCapacity != 128 || deployment.MaxBodyBytes != 50<<20 || deployment.MaxHeaderBytes != 32<<10 {
		t.Fatalf("deployment = %+v", deployment)
	}
}

func TestBrowserRolloutRequiresDistributedHostnameIsolation(t *testing.T) {
	// Neither DNS readiness nor an operator's enable flag substitutes for the
	// browser public-suffix boundary. Existing public/native startup stays valid.
	base := strings.TrimSuffix(validDeploymentJSON(), "}")
	if _, err := LoadDeployment(writeDeployment(t, base+`,"browser_access_enabled":true,"browser_login_origin":"https://login.example.test"}`)); err == nil {
		t.Fatal("enabled browser access without PSL isolation")
	}
	if _, err := LoadDeployment(writeDeployment(t, base+`,"browser_access_enabled":false,"browser_login_origin":"https://login.example.test"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDeploymentRejectsUnsafeProfiles(t *testing.T) {
	for _, mutate := range []func(string) string{
		func(value string) string {
			return strings.TrimSuffix(value, "}") + `,"public_https_cert_file":"/cert.pem"}`
		},
		func(value string) string {
			return strings.TrimSuffix(value, "}") + `,"public_https_cert_file":"relative","public_https_key_file":"/key.pem"}`
		},
		func(value string) string { return strings.Replace(value, "https://", "http://", 1) },
		func(value string) string {
			return strings.Replace(value, `"node_capacity":128`, `"node_capacity":0`, 1)
		},
		func(value string) string {
			return strings.Replace(value, `"preview_base_domain":"preview.example.test"`, `"preview_base_domain":"PREVIEW.example.test"`, 1)
		},
		func(value string) string {
			return strings.Replace(value, `"tunnel_base_domain":"tunnels.example.test"`, `"tunnel_base_domain":"example.test"`, 1)
		},
		func(value string) string { return strings.TrimSuffix(value, "}") + `,"unknown":true}` },
		func(value string) string {
			return strings.TrimSuffix(value, "}") + `,"node_capacity":64}`
		},
		func(value string) string {
			return strings.TrimSuffix(value, "}") + `,"public_routes":[{"host":"x.preview.example.test","upstream":"127.0.0.1:8080"}]}`
		},
		func(value string) string {
			return strings.TrimSuffix(value, "}") + `,"public_routes":[{"host":"api.example.test","host":"other.example.test","upstream":"127.0.0.1:8080"}]}`
		},
		func(value string) string {
			return strings.TrimSuffix(value, "}") + `,"public_routes":[{"host":"api.example.test","upstream":"8.8.8.8:80"}]}`
		},
		func(value string) string { return strings.TrimSuffix(value, "}") + `,"max_body_bytes":0}` },
		func(value string) string { return strings.TrimSuffix(value, "}") + `,"max_header_bytes":1023}` },
		func(value string) string { return strings.TrimSuffix(value, "}") + `,"max_header_bytes":131073}` },
	} {
		if _, err := LoadDeployment(writeDeployment(t, mutate(validDeploymentJSON()))); err == nil {
			t.Fatal("unsafe deployment accepted")
		}
	}
}

func TestLoadDeploymentAcceptsCustomGatewayLimits(t *testing.T) {
	value := strings.TrimSuffix(validDeploymentJSON(), "}") + `,"max_body_bytes":5368709120,"max_header_bytes":16384}`
	deployment, err := LoadDeployment(writeDeployment(t, value))
	if err != nil {
		t.Fatal(err)
	}
	if deployment.MaxBodyBytes != 5<<30 || deployment.MaxHeaderBytes != 16<<10 {
		t.Fatalf("gateway limits = body %d header %d", deployment.MaxBodyBytes, deployment.MaxHeaderBytes)
	}
}

func TestLoadDeploymentAcceptsBoundedPublicRoutes(t *testing.T) {
	value := strings.TrimSuffix(validDeploymentJSON(), "}") + `,"public_routes":[{"host":"api.example.test","path_prefix":"/helper-releases","strip_prefix":true,"upstream":"releases:8081"},{"host":"api.example.test","upstream":"server:8082"}]}`
	deployment, err := LoadDeployment(writeDeployment(t, value))
	if err != nil {
		t.Fatal(err)
	}
	if len(deployment.PublicRoutes) != 2 || deployment.PublicRoutes[0].PathPrefix != "/helper-releases" || !deployment.PublicRoutes[0].StripPrefix {
		t.Fatalf("public routes = %+v", deployment.PublicRoutes)
	}
}

func validDeploymentJSON() string {
	return `{"control_url":"https://edge-control.example.test","control_credential_file":"/opt/paperboat-tunnel/private/control.credential","jwks_file":"/opt/paperboat-tunnel/private/jwks.json","revocations_file":"/opt/paperboat-tunnel/private/revocations.json","usage_signing_key_file":"/opt/paperboat-tunnel/private/usage.key","connector_advertise_host":"edge.example.test","carrier_tcp_listen_address":"0.0.0.0:27443","carrier_quic_listen_address":"0.0.0.0:27444","public_https_listen_address":"127.0.0.1:18443","private_https_listen_address":"127.0.0.1:19443","public_http_listen_address":"127.0.0.1:18080","preview_base_domain":"preview.example.test","tunnel_base_domain":"tunnels.example.test","runtime_base_domain":"runtime.example.test","trusted_proxy_cidrs":["127.0.0.1/32"],"node_capacity":128,"control_interval":5000000000,"control_timeout":5000000000}`
}

func writeDeployment(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "deployment.json")
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSelfHostedStartsWithoutManagedDomains(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deployment.json")
	value := validDeploymentJSON()
	for _, domain := range []string{"preview.example.test", "tunnels.example.test", "runtime.example.test"} {
		value = strings.ReplaceAll(value, domain, "")
	}
	value = strings.Replace(value, `{`, `{"self_hosted":true,`, 1)
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDeployment(path); err != nil {
		t.Fatal(err)
	}
	value = strings.Replace(value, `"self_hosted":true`, `"self_hosted":false`, 1)
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDeployment(path); err == nil {
		t.Fatal("managed edge accepted unconfigured domains")
	}
}

func TestLoadDeploymentRetainsFileReadCause(t *testing.T) {
	_, err := LoadDeployment(t.TempDir())
	var original *os.PathError
	if !errors.As(err, &original) || original.Op != "read" || original.Err == nil {
		t.Fatalf("file read cause lost: %T", err)
	}
}

func TestDeploymentAllowsSharedPublicBaseAndRejectsRuntimeOverlap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deployment.json")
	shared := strings.ReplaceAll(validDeploymentJSON(), "preview.example.test", "public.example.test")
	shared = strings.ReplaceAll(shared, "tunnels.example.test", "public.example.test")
	if err := os.WriteFile(path, []byte(shared), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDeployment(path); err != nil {
		t.Fatal(err)
	}
	for _, runtime := range []string{"public.example.test", "nested.public.example.test"} {
		value := strings.ReplaceAll(shared, "runtime.example.test", runtime)
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadDeployment(path); err == nil {
			t.Fatal("runtime authority overlapped public namespace")
		}
	}
}

func TestPreviewCarrierIntervalDefaultsBelowControlInterval(t *testing.T) {
	deployment, err := LoadDeployment(writeDeployment(t, validDeploymentJSON()))
	if err != nil {
		t.Fatal(err)
	}
	if got := deployment.PreviewCarrierPollInterval(); got != DefaultPreviewCarrierInterval || got >= deployment.ControlInterval {
		t.Fatalf("default preview carrier interval = %s with control interval %s", got, deployment.ControlInterval)
	}
	explicit := strings.TrimSuffix(validDeploymentJSON(), "}") + `,"preview_carrier_interval":200000000}`
	deployment, err = LoadDeployment(writeDeployment(t, explicit))
	if err != nil || deployment.PreviewCarrierPollInterval() != 200*time.Millisecond {
		t.Fatalf("explicit preview carrier interval = %v, %v", deployment.PreviewCarrierPollInterval(), err)
	}
	for _, bad := range []string{`,"preview_carrier_interval":-1}`, `,"preview_carrier_interval":1000000}`, `,"preview_carrier_interval":3600000000000}`} {
		if _, err := LoadDeployment(writeDeployment(t, strings.TrimSuffix(validDeploymentJSON(), "}")+bad)); err == nil {
			t.Fatalf("accepted preview carrier interval %s", bad)
		}
	}
}
