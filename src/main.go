// Command argo-poc-app is a tiny web service used to demo Argo CD + Argo Rollouts.
// Each release shows its version, commit and a color derived from the version,
// so blue/green switches are easy to see. The page marker below is what e2e checks.
package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"html/template"
	"log"
	"net/http"
	"os"
)

// Set at build time with -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = "unknown"
)

var page = template.Must(template.New("page").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>argo-poc {{.Version}}</title>
<style>
body{margin:0;font-family:system-ui,sans-serif;background:hsl({{.Hue}},60%,92%);color:#1b1f24}
main{max-width:560px;margin:12vh auto;padding:32px;background:#fff;border-radius:12px;border-top:10px solid hsl({{.Hue}},65%,45%)}
h1{margin:0 0 8px}dt{color:#667;font-size:13px}dd{margin:0 0 12px;font-family:ui-monospace,monospace}
</style></head>
<body><main data-testid="argo-poc">
<h1>argo-poc {{.Version}}</h1>
<dl>
<dt>Environment</dt><dd>{{.Env}}</dd>
<dt>Commit</dt><dd>{{.Commit}}</dd>
<dt>Pod</dt><dd>{{.Pod}}</dd>
</dl>
<p style="color:#667;font-size:13px">Deployed by Argo CD from a management cluster, released blue-green by Argo Rollouts.</p>
</main></body></html>`))

func hue(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32() % 360
}

func main() {
	env := os.Getenv("APP_ENV")
	pod, _ := os.Hostname()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		data := map[string]any{"Version": version, "Commit": commit, "Env": env, "Pod": pod, "Hue": hue(version)}
		if err := page.Execute(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"version": version, "commit": commit, "env": env})
	})

	log.Printf("argo-poc %s (%s) listening on :8080", version, commit)
	log.Fatal(http.ListenAndServe(":8080", mux))
}
