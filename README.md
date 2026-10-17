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

## Promotion controller fixture

`deploy/pipeline.yaml` registers this project with the promotion controller. Its `test`, `staging` and `prod`
stages use `deploy/overlays/<stage>`, backed by the blue-green Rollout and e2e AnalysisTemplate in `deploy/base`.
The older `deploy/manifests` and `deploy/octopus` layouts remain available for the other PoCs.

The controller pins image digests and supplies release, commit and deployment annotations through Argo CD's
Kustomize overrides. The included transformer configurations propagate these overrides into the Rollout pod
template and the AnalysisTemplate Job. Git does not select image tags. Each local stage runs one replica.

CI validates PR builds without publishing. Main pushes and explicit workflow dispatches retain the `v0.2.<run>`
release tags and publish revision and creation annotations on the image index and platform manifests.

The promotion controller's e2e tests (`test/e2e` there) create releases by dispatching this workflow on `main`.
`e2e_id` names the run so a test finds it, and `e2e_fail=<stage>` builds a release whose e2e fails in that stage
(the `E2E_FAIL` build argument).
Images are pushed under a `tmp-<run id>` tag first and get their `v0.2.<run>` release tag together at the end of
the run, so a registry poller never sees one image of a build without the others.
