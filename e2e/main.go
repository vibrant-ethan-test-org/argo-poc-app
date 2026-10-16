// Command e2e runs end-to-end checks against a deployed argo-poc-app.
// The Harness pipeline runs it as a pod against the stage Service before swapping traffic.
// It exits non-zero if any check fails, which fails the stage and keeps the old version live.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

var client = &http.Client{Timeout: 5 * time.Second}

func get(url string) (int, string, error) {
	resp, err := client.Get(url)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), err
}

func main() {
	target := strings.TrimRight(os.Getenv("TARGET_URL"), "/")
	expectedSHA := os.Getenv("EXPECTED_SHA")
	expectedVersion := os.Getenv("EXPECTED_VERSION")
	if target == "" {
		fmt.Println("FAIL: TARGET_URL is not set")
		os.Exit(2)
	}
	fmt.Printf("e2e against %s (expected version %q, commit %q)\n", target, expectedVersion, expectedSHA)

	checks := []struct {
		name string
		run  func() error
	}{
		{"healthz returns 200", func() error {
			code, _, err := get(target + "/healthz")
			if err != nil {
				return err
			}
			if code != http.StatusOK {
				return fmt.Errorf("status %d", code)
			}
			return nil
		}},
		{"version matches the release being deployed", func() error {
			code, body, err := get(target + "/version")
			if err != nil {
				return err
			}
			if code != http.StatusOK {
				return fmt.Errorf("status %d", code)
			}
			var v struct{ Version, Commit string }
			if err := json.Unmarshal([]byte(body), &v); err != nil {
				return fmt.Errorf("bad JSON: %v", err)
			}
			if expectedVersion != "" && v.Version != expectedVersion {
				return fmt.Errorf("version %q, want %q", v.Version, expectedVersion)
			}
			if expectedSHA != "" && v.Commit != expectedSHA {
				return fmt.Errorf("commit %q, want %q", v.Commit, expectedSHA)
			}
			return nil
		}},
		{"home page renders", func() error {
			code, body, err := get(target + "/")
			if err != nil {
				return err
			}
			if code != http.StatusOK {
				return fmt.Errorf("status %d", code)
			}
			if !strings.Contains(body, `data-testid="argo-poc"`) {
				return fmt.Errorf("page is missing the argo-poc marker")
			}
			return nil
		}},
	}

	failed := 0
	for _, c := range checks {
		var err error
		// Retry briefly: the preview Service can take a moment to route to new pods.
		for attempt := 1; attempt <= 5; attempt++ {
			if err = c.run(); err == nil {
				break
			}
			time.Sleep(2 * time.Second)
		}
		if err != nil {
			failed++
			fmt.Printf("FAIL  %s: %v\n", c.name, err)
		} else {
			fmt.Printf("PASS  %s\n", c.name)
		}
	}
	if failed > 0 {
		fmt.Printf("%d check(s) failed\n", failed)
		os.Exit(1)
	}
	fmt.Println("all checks passed")
}
