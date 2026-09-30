package runexecutor

import (
	"errors"
	"strings"
	"testing"
)

func TestEnvironmentDoesNotInheritAmbientCredentials(t *testing.T) {
	t.Setenv("JOBFORGE_DATABASE_URL", "synthetic-control-secret")
	t.Setenv("HTTPS_PROXY", "http://untrusted.invalid")
	t.Setenv("PYTHONPATH", "/untrusted")
	env, err := childEnvironment(Environment{BusinessOrigin: "http://business:8090", BusinessReadKey: "synthetic-read-key", OllamaOrigin: "http://ollama:11434", DeepSeekKey: "synthetic-provider-key"})
	if err != nil {
		t.Fatal(err)
	}
	wantNames := map[string]bool{"PATH": true, "LANG": true, "PYTHONDONTWRITEBYTECODE": true, "JOBFORGE_BUSINESS_ORIGIN": true, "JOBFORGE_BUSINESS_READ_KEY": true, "JOBFORGE_OLLAMA_ORIGIN": true, "DEEPSEEK_API_KEY": true}
	if len(env) != len(wantNames) {
		t.Fatal("unexpected environment size")
	}
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if !wantNames[name] {
			t.Fatalf("unexpected environment name %q", name)
		}
	}
}

func TestEnvironmentRejectsCredentialAndOriginInjection(t *testing.T) {
	for _, env := range []Environment{
		{BusinessOrigin: "http://business", BusinessReadKey: "key\x00value"},
		{BusinessOrigin: "http://user:password@business", BusinessReadKey: "key"},
		{BusinessOrigin: "http://business/path", BusinessReadKey: "key"},
		{BusinessOrigin: "http://business?x=y", BusinessReadKey: "key"},
		{BusinessOrigin: "http://business"},
		{BusinessReadKey: "key"},
		{OllamaOrigin: "file:///tmp/socket"},
		{DeepSeekKey: "key\nOTHER=value"},
	} {
		if _, err := childEnvironment(env); !errors.Is(err, ErrEnvironment) {
			t.Fatal("accepted invalid deployment environment")
		}
	}
}

func TestDirectionClosure(t *testing.T) {
	for _, kind := range []string{"execute_step", "call_permit", "call_observation_ack", "call_intent", "call_observation", "step_result", "metering_report", "metering_ack", "unknown"} {
		if inbound(Ordinary, kind) && outbound(Ordinary, kind) {
			t.Fatalf("ordinary direction overlap for %s", kind)
		}
		if inbound(Metering, kind) && outbound(Metering, kind) {
			t.Fatalf("metering direction overlap for %s", kind)
		}
		if (inbound(Ordinary, kind) || outbound(Ordinary, kind)) && (inbound(Metering, kind) || outbound(Metering, kind)) {
			t.Fatalf("channel overlap for %s", kind)
		}
	}
	if inbound(Ordinary, "unknown") || outbound(Metering, "unknown") {
		t.Fatal("unknown direction accepted")
	}
}

func TestProviderNetworkIsExplicitAndScoped(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://ambient.invalid")
	t.Setenv("SSL_CERT_FILE", "/ambient/ca.pem")
	env, err := childEnvironment(Environment{DeepSeekProxyOrigin: "http://proxy:8080", DeepSeekCAFile: "/etc/jobforge/provider-ca.pem"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "JOBFORGE_DEEPSEEK_PROXY_ORIGIN=http://proxy:8080") || !strings.Contains(joined, "JOBFORGE_DEEPSEEK_CA_FILE=/etc/jobforge/provider-ca.pem") || strings.Contains(joined, "ambient") {
		t.Fatal("explicit provider network escaped its allowlist")
	}
	for _, pair := range [][2]string{
		{"http://proxy:8080", ""}, {"", "/etc/ca.pem"},
		{"http://user:password@proxy:8080", "/etc/ca.pem"},
		{"http://proxy:8080/path", "/etc/ca.pem"},
		{"http://proxy:8080?secret=x", "/etc/ca.pem"},
		{"socks5://proxy:8080", "/etc/ca.pem"},
		{"http://proxy:8080", "relative.pem"},
		{"http://proxy:8080", "/etc/../ca.pem"},
		{"http://proxy:8080", "/etc/ca.pem\nINJECT=value"},
	} {
		if _, err := childEnvironment(Environment{DeepSeekProxyOrigin: pair[0], DeepSeekCAFile: pair[1]}); !errors.Is(err, ErrEnvironment) {
			t.Fatal("unsafe provider network accepted")
		}
	}
}
