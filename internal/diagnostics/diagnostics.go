package diagnostics

import (
	"encoding/json"
	"strings"
)

const RedactedSensitiveContent = "[redacted sensitive content]"

func Redact(value string, forbidden []string) string {
	for _, secret := range forbidden {
		if secret == "" {
			continue
		}
		if strings.Contains(value, secret) {
			return RedactedSensitiveContent
		}
		escaped, err := json.Marshal(secret)
		if err == nil && len(escaped) >= 2 && strings.Contains(value, string(escaped[1:len(escaped)-1])) {
			return RedactedSensitiveContent
		}
	}
	return value
}
