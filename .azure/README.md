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
| Newest Freight wins | Exclusive lock check on `argo-poc-prod` (one deploy at a time) + the `guard` job: a run whose build is older than what prod runs skips its deploy with a warning |
| Promote an older Freight (rollback) | Open that run → Prod stage → **Rerun stage**. A re-run skips the guard |
| What is where | Pipelines → Environments → `argo-poc-test` / `argo-poc-prod` → Deployments; each run's summary links the app commit |

`lockBehavior: runLatest` alone is not enough: it only cancels older runs already waiting when a newer one asks for
the lock. When two held runs were approved together, the newer deployed first and the older then deployed over it.
The guard fixed that.

## Blocking promotion

All holds are checks on an environment: Pipelines → Environments → `argo-poc-prod` (or `argo-poc-test`) →
⋮ → **Approvals and checks**.

| To | Do | Undo |
|---|---|---|
| Hold every release at prod (freeze) | Enable the **Prod hold** check (⋮ → Enable). New runs stop before prod and wait up to 30 days | Disable the check. Runs started after that go straight through; runs already waiting still need **Approve** |
| Release one held build anyway | On the waiting run: **Review** → **Approve** | — |
| Block one release | On the waiting run: **Review** → **Reject**. Its Prod stage fails | Re-run the Prod stage (checks run again) |
| Keep a build in test | Enable the **Test hold** check on `argo-poc-test`. Newer runs wait before test, so test keeps the build it has | Disable it, and Approve or Reject waiting runs |

Holds also show on the run page as "Waiting: 1 check pending", and approvers get an email.

## Emergency release (skip test)

Run pipeline → pick the branch → tick **Emergency** → Run.

```
Build ─▶ Emergency sign-off (argo-poc-prod-emergency) ─▶ Prod (argo-poc-prod)
          Approvals check, always on                  guard, start, e2e, switch
```

- No test deploy and no soak. Prod still runs e2e against the new version before switching.
- Someone on the **Emergency sign-off** check must Approve. The run is tagged `emergency`, gets a warning naming
  who started it, and shows up under Environments → `argo-poc-prod-emergency` → Deployments (the audit log).
- If **Prod hold** is on, the run also needs that approval: approving it is the freeze override.
- The next normal release goes through test as usual. Test still has the older build until then.
- For real use, set "Approvers can't approve their own runs" on the sign-off check and add a Branch control check
  (for example only `main` and `hotfix/*`). In this PoC the sign-off approver is the only user, so self-approval is allowed.

## Trying it without breaking code

Run pipeline → branch or `main` → tick **Simulate a broken release** to make e2e fail in test. **Soak time**
changes the wait before prod. Manual runs deploy from any branch; automatic runs deploy only from `main`.

## One-time setup (already done)

- Environments `argo-poc-test` and `argo-poc-prod`, with pipeline 1 allowed to use them.
- `demo Build Service (ethanliu0087)` has Reader on `argo-poc-prod`, so the guard can read its deployments.
- `argo-poc-prod` checks: Exclusive lock (on), Prod hold (Approvals, disabled until needed).
- `argo-poc-test` checks: Test hold (Approvals, disabled until needed).
- `argo-poc-prod-emergency` checks: Emergency sign-off (Approvals, always on, 24h timeout).
