package relay

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// A provider may quote the access token anywhere in its reason, including
// across the 300 bytes the log keeps or the 4096 bytes the proxy reads of a
// body it passes on. No part of the token may reach the log.
func TestRejectionReasonsNeverQuoteAnyPartOfTheToken(t *testing.T) {
	token := "eyJhbGciOiJub25lIn0.eyJzdWIiOiJ0ZXN0In0." + strings.Repeat("s", 400)
	json := http.Header{"Content-Type": {"application/json"}}
	quoting := func(before int) []byte {
		return fmt.Appendf(nil, `{"error":{"type":"invalid_request_error","message":"%s Bearer %s"}}`, strings.Repeat("x", before), token)
	}
	// The reason starts with the type and a space, 22 bytes, and the log keeps 300.
	acrossTheShortening := quoting(260)
	// The proxy reads only the first 4096 bytes of a body it passes on.
	acrossTheRead := []byte(strings.Repeat(" ", 4000) + "Bearer " + token)[:4096]
	for name, body := range map[string][]byte{"across the shortening": acrossTheShortening, "across the read": acrossTheRead} {
		reason := describeRejection(json, body, token)
		if strings.Contains(reason, "Bearer "+token[:3]) || !strings.Contains(reason, "Bearer") {
			t.Errorf("%s: logged %q", name, reason)
		}
	}
}
