package services

import "strings"

func NewExternalSessionID(prefix string) string {
	prefix = sanitizeID(strings.TrimSpace(prefix))
	if prefix == "" || prefix == "unknown" {
		prefix = "chat"
	}
	return prefix + "_" + randomHex(10)
}
