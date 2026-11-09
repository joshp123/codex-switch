package providerreply

import (
	"encoding/base64"
	"net/url"
	"testing"
)

// A provider may put a token, in any form, where a code belongs. Only lowercase
// words pass, so none of it is shown.
func TestCodeKeepsProviderCodesAndNothingOfATokenInAnyForm(t *testing.T) {
	for _, code := range []string{"invalid_grant", "rate_limit_error", "refresh_token_reused", "websocket_connection_limit_reached", "max_output_tokens"} {
		if Code(code) != code {
			t.Errorf("code %q was not kept", code)
		}
	}
	token := "sk-ant-ort01-LiwpMn8KWrJBmLTchguVyR9Fxo7Zp115uGfmIwxHjsE"
	for _, form := range []string{
		token,
		base64.StdEncoding.EncodeToString([]byte(token)),
		url.QueryEscape(token),
		token[16:],
		"rt_other_machine_99d96fa9f1d5403b97683219ce64d2bc",
		"invalid_grant " + token,
		"invalid_grant\n",
	} {
		if Code(form) != "" {
			t.Errorf("%q passed as a code", form)
		}
	}
}

func TestRefusalGivesStatusCodesAndMeaningOnly(t *testing.T) {
	for body, want := range map[string]string{
		`{"error":"invalid_grant","error_description":"Refresh token sk-ant-ort01-abc not found"}`:                    "HTTP 400 invalid_grant: the refresh token is invalid, expired or revoked",
		`{"error":{"type":"invalid_request_error","code":"refresh_token_reused","message":"Your refresh token ..."}}`: "HTTP 400 invalid_request_error refresh_token_reused: the refresh token was already used",
		`{"error":{"type":"Bearer eyJhbGciOi","code":"c2stYW50LW9ydDAx","message":"..."}}`:                            "HTTP 400",
		`provider unavailable; Bearer eyJhbGciOiJSUzI1NiJ9`:                                                           "HTTP 400",
	} {
		if got := Refusal(400, []byte(body)); got != want {
			t.Errorf("Refusal(%s) = %q, want %q", body, got, want)
		}
	}
}
