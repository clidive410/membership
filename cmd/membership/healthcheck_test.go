package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func envMap(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestHealthcheckURL(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{}, "http://127.0.0.1:8080/healthz"},
		{map[string]string{"LISTEN_ADDR": ":9000"}, "http://127.0.0.1:9000/healthz"},
		{map[string]string{"LISTEN_ADDR": "[::]:9000"}, "http://127.0.0.1:9000/healthz"},
		{map[string]string{"LISTEN_ADDR": "10.0.0.5:8080"}, "http://10.0.0.5:8080/healthz"},
		{map[string]string{"BASE_URL": "https://clidive.org/membership/"}, "http://127.0.0.1:8080/membership/healthz"},
		{map[string]string{"HEALTHCHECK_URL": "http://x:1/healthz", "LISTEN_ADDR": ":9000"}, "http://x:1/healthz"},
	} {
		got, err := healthcheckURL(envMap(tc.env))
		if err != nil || got != tc.want {
			t.Errorf("%v: got %q, %v; want %q", tc.env, got, err, tc.want)
		}
	}
	if _, err := healthcheckURL(envMap(map[string]string{"LISTEN_ADDR": "nonsense"})); err == nil {
		t.Error("expected error for bad LISTEN_ADDR")
	}
}

func TestHealthcheck(t *testing.T) {
	status := http.StatusOK
	var path string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.WriteHeader(status)
	}))
	defer ts.Close()
	addr := strings.TrimPrefix(ts.URL, "http://")
	ctx := context.Background()

	if err := healthcheck(ctx, envMap(map[string]string{"LISTEN_ADDR": addr})); err != nil {
		t.Fatalf("healthy server: %v", err)
	}
	if path != "/healthz" {
		t.Errorf("probed %q", path)
	}
	if err := healthcheck(ctx, envMap(map[string]string{"HEALTHCHECK_URL": ts.URL + "/custom"})); err != nil || path != "/custom" {
		t.Errorf("HEALTHCHECK_URL: %v, path %q", err, path)
	}

	status = http.StatusServiceUnavailable
	if err := healthcheck(ctx, envMap(map[string]string{"LISTEN_ADDR": addr})); err == nil {
		t.Error("503 must fail")
	}

	ts.Close()
	if err := healthcheck(ctx, envMap(map[string]string{"LISTEN_ADDR": addr})); err == nil {
		t.Error("closed server must fail")
	}
}

func TestServeRejectsUnknownFlag(t *testing.T) {
	if err := serve(context.Background(), []string{"--nope"}); err == nil {
		t.Error("expected flag error")
	}
}
