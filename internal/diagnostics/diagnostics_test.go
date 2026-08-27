package diagnostics

import "testing"

func TestRedactHandlesDirectAndJSONEscapedSecrets(t *testing.T) {
	secret := "configured\nsecret"
	for _, value := range []string{"prefix " + secret + " suffix", `{"value":"configured\nsecret"}`} {
		if got := Redact(value, []string{secret}); got != RedactedSensitiveContent {
			t.Fatalf("Redact(%q) = %q", value, got)
		}
	}
	if got := Redact("safe", []string{"", secret}); got != "safe" {
		t.Fatalf("Redact(safe) = %q", got)
	}
}
