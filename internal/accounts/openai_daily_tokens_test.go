package accounts

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var chatGPTOrigin = &url.URL{Scheme: "https", Host: "chatgpt.com"}

func TestOpenAIDailyTokenSourceReadsOnlyValidatedAccountBuckets(t *testing.T) {
	home := t.TempDir()
	access := jwt(t, time.Now().Add(time.Hour), "account-a")
	writeTestAuth(t, home, access, "refresh-a", "account-a")
	before, modified := authFileState(t, home)

	var request *http.Request
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(value *http.Request) (*http.Response, error) {
		requests++
		request = value
		return tokenResponse(http.StatusOK, `{
			"account_id":"untrusted-response-identity",
			"metadata":{"generated_at":"2026-07-23T08:00:00Z","stats_as_of":"2026-07-23"},
			"stats":{
				"lifetime_tokens":12,
				"daily_usage_buckets":[
					{"start_date":"2026-07-22","tokens":7},
					{"start_date":"2026-07-20","tokens":5}
				]
			}
		}`), nil
	})}

	history, err := NewOpenAIDailyTokenSource(home, chatGPTOrigin, client).Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if request == nil || request.Method != http.MethodGet || request.URL.String() != "https://chatgpt.com"+openAIDailyTokenProfilePath {
		t.Fatalf("request = %#v", request)
	}
	if requests != 1 {
		t.Fatalf("OpenAI profile requests = %d", requests)
	}
	if request.Header.Get("Authorization") != "Bearer "+access ||
		request.Header.Get("ChatGPT-Account-ID") != "account-a" ||
		request.Header.Get("User-Agent") != "codex-cli" {
		t.Fatalf("OpenAI profile headers = %#v", request.Header)
	}
	if history.AccountID != "account-a" {
		t.Fatalf("account identity = %q", history.AccountID)
	}
	if history.LifetimeTokens != 12 {
		t.Fatalf("lifetime tokens = %d", history.LifetimeTokens)
	}
	want := []OpenAIDailyTokenBucket{
		{StartDate: "2026-07-20", Tokens: 5},
		{StartDate: "2026-07-22", Tokens: 7},
	}
	if got := history.Buckets; !reflect.DeepEqual(got, want) {
		t.Fatalf("buckets = %#v, want %#v", got, want)
	}

	assertAuthFileState(t, home, before, modified)
	if _, err := os.Stat(filepath.Join(home, "auth.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only OpenAI history created auth.lock: %v", err)
	}
}

func TestOpenAIDailyTokenHistoryReconcilesValidatedBucketsWithOpenAILifetime(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "missing stats", body: `{"metadata":{"stats_as_of":"2026-07-23","generated_at":"2026-07-23T08:00:00Z"}}`},
		{name: "missing lifetime", body: `{"metadata":{"stats_as_of":"2026-07-23","generated_at":"2026-07-23T08:00:00Z"},"stats":{"daily_usage_buckets":[]}}`},
		{name: "missing buckets", body: `{"metadata":{"stats_as_of":"2026-07-23","generated_at":"2026-07-23T08:00:00Z"},"stats":{"lifetime_tokens":0}}`},
		{name: "negative lifetime", body: `{"metadata":{"stats_as_of":"2026-07-23","generated_at":"2026-07-23T08:00:00Z"},"stats":{"lifetime_tokens":-1,"daily_usage_buckets":[]}}`},
		{name: "invalid date", body: `{"metadata":{"stats_as_of":"2026-07-23","generated_at":"2026-07-23T08:00:00Z"},"stats":{"lifetime_tokens":1,"daily_usage_buckets":[{"start_date":"not-a-date","tokens":1}]}}`},
		{name: "padded date", body: `{"metadata":{"stats_as_of":"2026-07-23","generated_at":"2026-07-23T08:00:00Z"},"stats":{"lifetime_tokens":1,"daily_usage_buckets":[{"start_date":" 2026-07-20 ","tokens":1}]}}`},
		{name: "duplicate date", body: `{"metadata":{"stats_as_of":"2026-07-23","generated_at":"2026-07-23T08:00:00Z"},"stats":{"lifetime_tokens":2,"daily_usage_buckets":[{"start_date":"2026-07-20","tokens":1},{"start_date":"2026-07-20","tokens":1}]}}`},
		{name: "missing tokens", body: `{"metadata":{"stats_as_of":"2026-07-23","generated_at":"2026-07-23T08:00:00Z"},"stats":{"lifetime_tokens":0,"daily_usage_buckets":[{"start_date":"2026-07-20"}]}}`},
		{name: "negative tokens", body: `{"metadata":{"stats_as_of":"2026-07-23","generated_at":"2026-07-23T08:00:00Z"},"stats":{"lifetime_tokens":0,"daily_usage_buckets":[{"start_date":"2026-07-20","tokens":-1}]}}`},
		{name: "lifetime mismatch", body: `{"metadata":{"stats_as_of":"2026-07-23","generated_at":"2026-07-23T08:00:00Z"},"stats":{"lifetime_tokens":999,"daily_usage_buckets":[{"start_date":"2026-07-20","tokens":1}]}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeOpenAIDailyTokenHistory("account-a", time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC), []byte(test.body)); err == nil {
				t.Fatal("invalid OpenAI snapshot was accepted")
			}
		})
	}

	empty, err := decodeOpenAIDailyTokenHistory("account-a", time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC), []byte(
		`{"metadata":{"stats_as_of":"2026-07-23","generated_at":"2026-07-23T08:00:00Z"},"stats":{"lifetime_tokens":0,"daily_usage_buckets":[]}}`,
	))
	if err != nil || empty.AccountID != "account-a" || empty.LifetimeTokens != 0 || len(empty.Buckets) != 0 {
		t.Fatalf("zero-token snapshot = %#v, %v", empty, err)
	}
	if _, err := decodeOpenAIDailyTokenHistory(" ", time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC), []byte(
		`{"metadata":{"stats_as_of":"2026-07-23","generated_at":"2026-07-23T08:00:00Z"},"stats":{"lifetime_tokens":0,"daily_usage_buckets":[]}}`,
	)); err == nil {
		t.Fatal("snapshot without an authenticated account was accepted")
	}
}

func TestDailyTokenHistoryKeepsProviderReportingDate(t *testing.T) {
	history, err := decodeOpenAIDailyTokenHistory("account-a", time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC), []byte(`{
  "metadata":{"stats_as_of":"2026-08-01","generated_at":"2026-08-02T00:10:00Z"},
  "stats":{"lifetime_tokens":1062,"daily_usage_buckets":[
   {"start_date":"2026-07-02","tokens":2},
   {"start_date":"2026-07-03","tokens":3},
   {"start_date":"2026-07-26","tokens":26},
   {"start_date":"2026-07-31","tokens":31},
   {"start_date":"2026-08-01","tokens":1000}
  ]}
 }`))
	if err != nil {
		t.Fatal(err)
	}
	if got := history.ReportingDate.Format("2006-01-02"); got != "2026-08-01" {
		t.Fatalf("provider reporting date = %s", got)
	}
	if got := history.GeneratedAt.Format(time.RFC3339); got != "2026-08-02T00:10:00Z" {
		t.Fatalf("provider generation time = %s", got)
	}
}

func TestOpenAIDailyTokenHistoryRequiresProviderReportingMetadata(t *testing.T) {
	for _, metadata := range []string{
		`null`,
		`{}`,
		`{"stats_as_of":"not-a-date","generated_at":"2026-07-23T08:00:00Z"}`,
		`{"stats_as_of":"2026-07-23"}`,
		`{"stats_as_of":"2026-07-23","generated_at":"2026-07-23T08:00:00Z","stats_error":"failure"}`,
	} {
		body := `{"metadata":` + metadata + `,"stats":{"lifetime_tokens":0,"daily_usage_buckets":[]}}`
		if _, err := decodeOpenAIDailyTokenHistory("account-a", time.Now(), []byte(body)); err == nil {
			t.Fatalf("accepted invalid provider metadata: %s", metadata)
		}
	}
}

func TestOpenAIDailyTokenSourceRejectsExpiringCredentialsWithoutOpenAIOrAuthWrite(t *testing.T) {
	home := t.TempDir()
	writeTestAuth(t, home, jwt(t, time.Now().Add(time.Minute), "account-a"), "refresh-a", "account-a")
	before, modified := authFileState(t, home)

	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("OpenAI must not be called with expiring credentials")
	})}

	if _, err := NewOpenAIDailyTokenSource(home, chatGPTOrigin, client).Read(t.Context()); !requiresSignIn(err) {
		t.Fatalf("expired credentials error = %T %v", err, err)
	}
	if requests != 0 {
		t.Fatalf("OpenAI profile requests = %d", requests)
	}
	assertAuthFileState(t, home, before, modified)
}

func TestOpenAIDailyTokenSourceRejects401WithoutRetryOrAuthWrite(t *testing.T) {
	home := t.TempDir()
	writeTestAuth(t, home, jwt(t, time.Now().Add(time.Hour), "account-a"), "refresh-a", "account-a")
	before, modified := authFileState(t, home)
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return tokenResponse(http.StatusUnauthorized, `{"detail":"private OpenAI response"}`), nil
	})}

	_, err := NewOpenAIDailyTokenSource(home, chatGPTOrigin, client).Read(t.Context())
	if !requiresSignIn(err) || strings.Contains(err.Error(), "private OpenAI response") {
		t.Fatalf("401 error = %T %v", err, err)
	}
	if requests != 1 {
		t.Fatalf("OpenAI profile requests = %d", requests)
	}
	assertAuthFileState(t, home, before, modified)
}

func TestOpenAIDailyTokenSourceRejectsRedirectsWithoutFollowingOrAuthWrite(t *testing.T) {
	for _, status := range []int{
		http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			home := t.TempDir()
			writeTestAuth(t, home, jwt(t, time.Now().Add(time.Hour), "account-a"), "refresh-a", "account-a")
			before, modified := authFileState(t, home)
			requests := 0
			body := &trackingReadCloser{Reader: strings.NewReader("private redirect response")}
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				requests++
				return &http.Response{
					StatusCode: status,
					Body:       body,
					Header:     http.Header{"Location": []string{"https://redirect.invalid"}},
					Request:    request,
				}, nil
			})}

			_, err := NewOpenAIDailyTokenSource(home, chatGPTOrigin, client).Read(t.Context())
			if err == nil || strings.Contains(err.Error(), "private redirect response") {
				t.Fatalf("redirect error = %v", err)
			}
			if requests != 1 {
				t.Fatalf("OpenAI profile requests = %d", requests)
			}
			if !body.closed {
				t.Fatal("redirect response body was not closed")
			}
			if client.CheckRedirect != nil {
				t.Fatal("daily token source changed the shared client")
			}
			assertAuthFileState(t, home, before, modified)
		})
	}
}

func TestOpenAIDailyTokenSourceDoesNotExposeOpenAIFailureBody(t *testing.T) {
	home := t.TempDir()
	writeTestAuth(t, home, jwt(t, time.Now().Add(time.Hour), "account-a"), "refresh-a", "account-a")
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return tokenResponse(http.StatusBadGateway, `{"detail":"private OpenAI response"}`), nil
	})}
	_, err := NewOpenAIDailyTokenSource(home, chatGPTOrigin, client).Read(t.Context())
	if err == nil || strings.Contains(err.Error(), "private OpenAI response") {
		t.Fatalf("OpenAI error = %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func tokenResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

type trackingReadCloser struct {
	io.Reader
	closed bool
}

func (r *trackingReadCloser) Close() error {
	r.closed = true
	return nil
}

func authFileState(t *testing.T, home string) ([]byte, time.Time) {
	t.Helper()
	path := filepath.Join(home, "auth.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return data, info.ModTime()
}

func assertAuthFileState(t *testing.T, home string, want []byte, wantModified time.Time) {
	t.Helper()
	got, modified := authFileState(t, home)
	if !bytes.Equal(got, want) {
		t.Fatal("daily token read changed auth.json bytes")
	}
	if !modified.Equal(wantModified) {
		t.Fatal("daily token read changed auth.json modification time")
	}
}
