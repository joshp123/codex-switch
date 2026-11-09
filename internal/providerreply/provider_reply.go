// Package providerreply describes what OpenAI and Anthropic send back in
// words this program wrote. A provider's text can quote a credential whole,
// encoded or cut, so no message, body, header value or status line a provider
// sends reaches the proxy log, command output, status or the menu. What does
// reach them is the HTTP status, a provider's error code when it is a short
// identifier such as invalid_grant, and a sentence this package wrote for each
// code it knows.
package providerreply

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"slices"
	"strings"
	"syscall"
)

// identifier is a provider's error code: lowercase words joined by "_", "."
// or "-". It has no digits or capitals, so no part of a token is one.
var identifier = regexp.MustCompile(`^[a-z]+(?:[_.-][a-z]+)*$`)

const longestIdentifier = 64

// Code is value when it is a provider's error code, such as invalid_grant or
// rate_limit_error, and "" otherwise.
func Code(value string) string {
	if len(value) > longestIdentifier || !identifier.MatchString(value) {
		return ""
	}
	return value
}

// meanings are the codes whose meaning OpenAI or Anthropic document, or that
// Codex and Claude Code act on.
var meanings = map[string]string{
	// RFC 6749, section 5.2; OpenAI and Anthropic return it for a refresh
	// token they no longer accept.
	"invalid_grant": "the refresh token is invalid, expired or revoked",
	// OpenAI's refresh failures, as Codex reads them.
	"refresh_token_expired":     "the refresh token has expired",
	"refresh_token_reused":      "the refresh token was already used",
	"refresh_token_invalidated": "the refresh token was revoked",
	// Anthropic's API errors.
	"authentication_error": "the credentials were not accepted",
	"permission_error":     "the credentials do not allow this request",
	"rate_limit_error":     "the provider is limiting this account's requests",
	"overloaded_error":     "the provider is overloaded",
	"api_error":            "an error inside the provider",
	// OpenAI's errors.
	"server_error":        "an error inside the provider",
	"usage_limit_reached": "the account's usage limit is reached",
}

// Codes is each of values that is a code, once, then what the first code with
// a known meaning means: "invalid_grant: the refresh token is invalid,
// expired or revoked". It is "" when no value is a code.
func Codes(values ...string) string {
	var codes []string
	meaning := ""
	for _, value := range values {
		code := Code(value)
		if code == "" || slices.Contains(codes, code) {
			continue
		}
		codes = append(codes, code)
		if meaning == "" {
			meaning = meanings[code]
		}
	}
	described := strings.Join(codes, " ")
	if meaning != "" {
		described += ": " + meaning
	}
	return described
}

// Refusal describes an error reply by its HTTP status and the codes in its
// body: "HTTP 400 invalid_grant: the refresh token is invalid, expired or
// revoked".
func Refusal(status int, body []byte) string {
	described := fmt.Sprintf("HTTP %d", status)
	if codes := Codes(ReplyCodes(body)...); codes != "" {
		described += " " + codes
	}
	return described
}

// ReplyCodes is each code in an error reply, in the shapes OpenAI and
// Anthropic use: {"error": "code"} and {"error": {"type": "...", "code":
// "..."}}.
func ReplyCodes(body []byte) []string {
	var reply struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &reply) != nil {
		return nil
	}
	var code string
	var apiError struct {
		Type string `json:"type"`
		Code string `json:"code"`
	}
	if json.Unmarshal(reply.Error, &code) != nil {
		_ = json.Unmarshal(reply.Error, &apiError)
	}
	var codes []string
	for _, value := range []string{code, apiError.Type, apiError.Code} {
		if Code(value) != "" && !slices.Contains(codes, value) {
			codes = append(codes, value)
		}
	}
	return codes
}

// Failure describes why an exchange with a provider failed. Go's errors
// quote what the provider sent in a malformed reply, so only the kind of
// failure is kept, with the system's own description of a socket error.
func Failure(err error) string {
	var lookup *net.DNSError
	var certificate *tls.CertificateVerificationError
	var socket syscall.Errno
	var timeout interface{ Timeout() bool }
	switch {
	case errors.Is(err, context.Canceled):
		return "the request was cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "the provider did not answer in time"
	case errors.As(err, &lookup):
		return "the provider's address could not be looked up"
	case errors.As(err, &certificate):
		return "the provider's TLS certificate is not trusted"
	case errors.As(err, &socket):
		return "the connection failed: " + socket.Error()
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "the provider closed the connection before its reply ended"
	case NotHTTP(err):
		return "the provider's reply was not valid HTTP"
	case errors.As(err, &timeout) && timeout.Timeout():
		return "the provider did not answer in time"
	}
	return exchangeFailed
}

const exchangeFailed = "the exchange with the provider failed"

// NotHTTP reports whether err is Go's client finding that a reply was not
// HTTP, as from a broken proxy or server.
func NotHTTP(err error) bool {
	return strings.Contains(err.Error(), "malformed HTTP ") || strings.Contains(err.Error(), "server gave HTTP response to HTTPS client")
}

// Decode reads a provider's JSON reply, one JSON value, into value. Its error
// names only what went wrong: Go's decoding errors can quote the reply.
func Decode(reply io.Reader, value any) error {
	decoder := json.NewDecoder(reply)
	err := decoder.Decode(value)
	if err == nil && decoder.Decode(&json.RawMessage{}) != io.EOF {
		return errors.New("the reply has more after its JSON")
	}
	var syntax *json.SyntaxError
	var wrongType *json.UnmarshalTypeError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &syntax), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return errors.New("the reply is not complete JSON")
	case errors.As(err, &wrongType):
		return fmt.Errorf("the reply's %s field is not a %s", wrongType.Field, wrongType.Type)
	}
	if failure := Failure(err); failure != exchangeFailed {
		return errors.New("the reply could not be read: " + failure)
	}
	return errors.New("a value in the reply is not in the form expected")
}
