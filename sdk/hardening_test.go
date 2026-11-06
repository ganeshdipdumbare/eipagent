package sdk

import (
	"net"
	"strings"
	"testing"
)

func TestCheckAddressAlwaysRejectsMetadataAndLinkLocal(t *testing.T) {
	for _, s := range []string{
		"169.254.169.254", "169.254.170.2", "169.254.169.23",
		"100.100.100.200", "192.0.0.192", "168.63.129.16",
		"fd00:ec2::254", "fe80::1",
	} {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test address %s", s)
		}
		if err := checkAddress(ip, true); err == nil {
			t.Errorf("checkAddress(%s, allowPrivate=true) accepted metadata/link-local", s)
		}
	}
}

func TestChildEnvSkipsDangerousAndSecretNames(t *testing.T) {
	env := childEnv(ProcOptions{Env: map[string]string{
		"GOOD": "1", "LD_PRELOAD": "/tmp/x.so", "MY_API_KEY": "secret",
	}})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "LD_PRELOAD") || strings.Contains(joined, "MY_API_KEY") {
		t.Fatalf("dangerous or secret env leaked to child: %v", env)
	}
	if !strings.Contains(joined, "GOOD=1") {
		t.Fatalf("safe env missing: %v", env)
	}
}

func TestSecretLikeCatchesCommonCredentials(t *testing.T) {
	for _, name := range []string{
		"AWS_SECRET_ACCESS_KEY", "GITHUB_TOKEN", "CLIENT_SECRET",
		"JWT_SIGNING_KEY", "MONGODB_URI", "AUTHORIZATION", "DB_PASSWORD",
		"STRIPE_WEBHOOK_SECRET", "TLS_PRIVATE_KEY", "REDIS_URL",
		"PG_URL", "KUBECONFIG", "SLACK_WEBHOOK_URL",
	} {
		if !SecretLike(name) {
			t.Errorf("SecretLike(%q) = false", name)
		}
	}
	for _, name := range []string{"PATH", "HOME", "LANG", "TERM", "PORT"} {
		if SecretLike(name) {
			t.Errorf("SecretLike(%q) = true", name)
		}
	}
}
