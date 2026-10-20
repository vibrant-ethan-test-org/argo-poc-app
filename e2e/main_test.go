package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Run the actual CLI so the regression covers its exit status as well as its checks.
func TestExpectedSHA(t *testing.T) {
	if os.Getenv("FIXTURE_E2E_HELPER") == "1" {
		main()
		return
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"Version":"v0.2.1","Commit":"abc123"}`)
		case "/api/info":
			fmt.Fprint(w, `{"Service":"argo-poc-api"}`)
		case "/":
			fmt.Fprint(w, `<div data-testid="argo-poc"></div>`)
		}
	}))
	defer server.Close()
	for _, tc := range []struct {
		name, sha, message string
		wantSuccess        bool
	}{
		{"missing", "", "FAIL: EXPECTED_SHA is required", false},
		{"matching", "abc123", "all checks passed", true},
		{"mismatched", "other", `commit "abc123", want "other"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExpectedSHA$")
			for _, env := range os.Environ() {
				if strings.HasPrefix(env, "FIXTURE_E2E_HELPER=") || strings.HasPrefix(env, "TARGET_URL=") || strings.HasPrefix(env, "EXPECTED_SHA=") || strings.HasPrefix(env, "EXPECTED_VERSION=") {
					continue
				}
				cmd.Env = append(cmd.Env, env)
			}
			cmd.Env = append(cmd.Env, "FIXTURE_E2E_HELPER=1", "TARGET_URL="+server.URL, "EXPECTED_SHA="+tc.sha, "EXPECTED_VERSION=v0.2.1")
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatal("e2e process exceeded deadline")
			}
			if (err == nil) != tc.wantSuccess || !strings.Contains(string(output), tc.message) {
				t.Fatalf("exit=%v output=%s", err, output)
			}
		})
	}
}
