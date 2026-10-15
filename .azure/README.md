# Azure Pipelines version

`azure-pipelines.yml` runs the same release flow as the Kargo setup in
[argo-poc-gitops](https://github.com/vibrant-ethan-test-org/argo-poc-gitops), in Azure DevOps
([ethanliu0087/demo](https://dev.azure.com/ethanliu0087/demo/_build?definitionId=1)). There is no
cluster: a "deploy" starts the image on the build agent and runs the real e2e image against it.

```
push to main ─▶ Build ─▶ Test (argo-poc-test) ─▶ soak 1m ─▶ Prod (argo-poc-prod)
                images     start, e2e, switch                  start, e2e, switch
```

| Kargo | Azure Pipelines |
|---|---|
| Freight (one build) | Build stage saves both images as the `images` artifact; test and prod load that same artifact |
| Blue-green + e2e gate | `.azure/deploy-steps.yml`: start new version, e2e against it, then "switch traffic" (simulated). e2e failing fails the stage and prod never runs |
| Auto-promote with 1m soak | `Prod` depends on `Test`; `Delay` at the end of `Test` (server job, no agent held) |
| Newest Freight wins | Exclusive lock check on `argo-poc-prod` + `lockBehavior: runLatest`: older runs waiting for prod are cancelled |
| What is where | Pipelines → Environments → `argo-poc-test` / `argo-poc-prod` → Deployments; each run's summary links the app commit |

## Blocking promotion

All holds are checks on an environment: Pipelines → Environments → `argo-poc-prod` (or `argo-poc-test`) →
⋮ → **Approvals and checks**.

| To | Do | Undo |
|---|---|---|
| Hold every release at prod (freeze) | Enable the **Prod hold** check (⋮ → Enable). New runs stop before prod and wait up to 30 days | Disable the check. Runs started after that go straight through; runs already waiting still need **Approve** |
| Release one held build anyway | On the waiting run: **Review** → **Approve** | — |
| Block one release for good | On the waiting run: **Review** → **Reject**. Prod is skipped for that run | Re-run the Prod stage |
| Keep a build in test | Enable the **Test hold** check on `argo-poc-test`. Newer runs wait before test, so test keeps the build it has | Disable it, and Approve or Reject waiting runs |

Holds also show on the run page as "Waiting: 1 check pending", and approvers get an email.

## Trying it without breaking code

Run pipeline → branch or `main` → tick **Simulate a broken release** to make e2e fail in test. **Soak time**
changes the wait before prod. Manual runs deploy from any branch; automatic runs deploy only from `main`.

## One-time setup (already done)

- Environments `argo-poc-test` and `argo-poc-prod`, with pipeline 1 allowed to use them.
- `argo-poc-prod` checks: Exclusive lock (on), Prod hold (Approvals, disabled until needed).
- `argo-poc-test` checks: Test hold (Approvals, disabled until needed).
