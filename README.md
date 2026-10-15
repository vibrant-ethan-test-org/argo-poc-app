# argo-poc-app

Demo app for the Argo CD + Argo Rollouts proof of concept. Deployment config, setup and the
full walkthrough live in [argo-poc-gitops](https://github.com/vibrant-ethan-test-org/argo-poc-gitops).

| Path | What |
|---|---|
| `src/` | Go web service: `/` shows version, commit and environment; `/healthz`; `/version` (JSON) |
| `e2e/` | e2e checks run by Argo Rollouts against the preview Service before traffic switches |
| `.github/workflows/ci.yaml` | Calls the shared `release.yaml` in the gitops repo: builds multi-arch images to GHCR as `v0.1.<run>`, then commits the tags to the gitops test overlay |

Where each commit is deployed shows in this repo's **Environments** panel (test, prod), recorded by
the gitops repo's `on-deployed.yaml` when Argo CD reports a Healthy deploy.

Run locally:

```sh
docker build -t argo-poc-app --build-arg VERSION=dev --build-arg COMMIT=local .
docker run --rm -p 8080:8080 argo-poc-app
```
