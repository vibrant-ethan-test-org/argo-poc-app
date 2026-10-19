package main

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestStagingHealthFailure(t *testing.T) {
	for _, tt := range []struct {
		env     string
		delay   time.Duration
		started time.Time
		want    int
	}{
		{"fixture-staging", 20 * time.Second, time.Now().Add(-time.Minute), 503},
		{"fixture-staging", 20 * time.Second, time.Now().Add(time.Hour), 200},
		{"fixture-test", 20 * time.Second, time.Now().Add(-time.Minute), 200},
		{"fixture-prod", 20 * time.Second, time.Now().Add(-time.Minute), 200},
		{"fixture-staging", 0, time.Now().Add(-time.Minute), 200},
	} {
		r := httptest.NewRecorder()
		healthHandler(tt.env, tt.delay, tt.started)(r, httptest.NewRequest("GET", "/healthz", nil))
		if r.Code != tt.want {
			t.Errorf("%s delay %s: status %d, want %d", tt.env, tt.delay, r.Code, tt.want)
		}
	}
}
