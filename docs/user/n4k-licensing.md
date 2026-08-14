# N4K Licensing — Support Guide

Licensing for Nirmata Enterprise Kyverno (N4K), written for Customer Success and
Support. Covers what the product enforces, how to apply a license, how to read a
cluster's licensing state, and how to triage the cases customers actually report.

Applies to chart `3.8.0-rc.4` / N4K `v1.18.0-n4k.nirmata.1` and later.

Engineering reference: [`pkg/nirmata/license/README.md`](../../pkg/nirmata/license/README.md).
Helm values reference: [`charts/kyverno/README.md`](../../charts/kyverno/README.md).

---

## 1. The short version

N4K checks its license offline — no callbacks to Nirmata, works in air-gapped
clusters. Every install has a license: either the commercial one the customer
mounts, or the free-tier community license embedded in the image.

**The only thing licensing ever refuses is the creation of a *new policy* past the
licensed policy cap, and only in three of the six states.**

Never affected, in any state:

- **Workload admission.** Pods, Deployments, and everything else keep being
  validated, mutated and generated. Licensing is not in that path.
- **Existing policies.** They keep enforcing. Nothing is disabled or turned to Audit.
- **Updating or deleting policies.** Only CREATE is gated — a customer at the cap
  can always edit or delete their way back under it.
- **Pod health.** A missing, expired, or corrupt license never crashes or blocks
  startup. The system fails open by design.

So "our license expired" never means "our cluster stopped admitting workloads."
If a customer reports an outage and blames licensing, licensing is almost
certainly not the cause — see §7.

---

## 2. What a license grants

| | Free tier (built in) | Commercial (issued by Nirmata) |
|---|---|---|
| Policies | **10** | Per contract (`max_policies`) |
| Node units | **10** | Per contract (`max_node_units`) |
| Window | **15-day trial** from first install | `not_before` → `not_after` in the license |
| Grace after non-compliance | None | Per contract (`grace_days`, typically 30) |
| Cost of exceeding | New policy CREATE refused | Nothing refused until grace runs out |

**Policy count** = every Kyverno policy kind, cluster-scoped and namespaced, so a
cap on `ClusterPolicy` can't be worked around by authoring `ValidatingPolicy`
instead. Full list in §10.

**Node unit** = `ceil(node memory in GiB ÷ 33)`, summed over all nodes. A 64 GiB
node is 2 units; a 16 GiB node is 1. The 33 is 32 GiB plus 1 GiB of headroom, so a
node advertising slightly under 32 (every node does, after kubelet reservations)
still counts as one unit. This matches how license-guardian counts, so N4K and the
contract agree.

---

## 3. The six states

Ordered from healthy to worst. The state is what everything else keys off.

| State | Means | New policy over cap | Everything else |
|---|---|---|---|
| `Licensed` | Valid commercial license, comfortably inside its window and under 90% of capacity | Allowed | Normal |
| `FreeTier` | No commercial license, inside the 15-day trial | **Refused** | Normal |
| `Warning` | Valid commercial license within 30 days of expiry, or at 90–100% of a cap | Allowed | Normal + warnings |
| `Grace` | Commercial license expired, over capacity, invalid, or not yet in force — inside the grace window | Allowed | Normal + warnings |
| `FreeExpired` | No commercial license, 15-day trial elapsed | **Refused** | Normal + loud warnings |
| `Degraded` | Commercial non-compliance, grace window elapsed | **Refused** | Normal + loud warnings |

Two things worth internalizing, because they surprise people:

**Going over the cap on a paid license does not block anything.** It moves the
cluster to `Grace` — full function, warnings raised — and starts a grace clock.
The customer has the whole grace window (typically 30 days) to buy more capacity
or delete policies. Only `Degraded`, after that window, refuses new policies.

**The grace clock starts at first detection, not at expiry.** If a license expired
three months ago but the cluster only came online yesterday, the customer gets a
full grace window from yesterday. It is recorded in the cluster and survives
restarts and upgrades.

---

## 4. Installing with a license

The license is a signed text file (starts with `nctl-license-v1`). Three ways to
get it into a cluster; the first is the default recommendation.

### Option A — Helm creates the Secret

```bash
helm upgrade --install kyverno nirmata/kyverno -n kyverno --create-namespace \
  --set license.create=true \
  --set-file license.value=license.lic
```

Or in a values file:

```yaml
license:
  create: true
  value: |
    nctl-license-v1
    eyJjdXN0b21lciI6...
    <signature>
```

### Option B — customer already has the Secret

```bash
kubectl -n kyverno create secret generic my-license --from-file=license=license.lic

helm upgrade --install kyverno nirmata/kyverno -n kyverno \
  --set license.existingSecret=my-license
```

### Option C — custom mount path (Vault, CSI, pre-baked image)

Only when the customer mounts the file themselves. `features.licensing.licensePath`
tells N4K where to look; it does **not** deliver the file.

```yaml
features:
  licensing:
    licensePath: /etc/nirmata/license
admissionController:
  extraVolumes:
    - name: nirmata-license
      csi:
        driver: secrets-store.csi.k8s.io
        readOnly: true
        volumeAttributes: {secretProviderClass: nirmata-license}
  extraVolumeMounts:
    - name: nirmata-license
      mountPath: /etc/nirmata
      readOnly: true
```

Leave `license.create` and `license.existingSecret` unset here, or the chart adds a
second mount that conflicts.

> **Common mistake:** setting `features.licensing.licensePath` *without* mounting
> anything. N4K then looks at a path that doesn't exist, logs one line, and falls
> back to the free tier — the customer sees a cap of 10 and thinks their license
> didn't work. It didn't: nothing delivered the file.

### Renewing or replacing a license

Same command with the new file. No restart needed — the new license is picked up at
the next evaluation (within 15 minutes by default). To make it immediate:

```bash
kubectl -n kyverno rollout restart deploy/kyverno-admission-controller
```

### Verifying it took

```bash
# 1. flag points at the file
kubectl -n kyverno get deploy kyverno-admission-controller -o yaml | grep -- --licensePath

# 2. file is actually in the container
kubectl -n kyverno exec deploy/kyverno-admission-controller -c kyverno -- \
  head -1 /var/run/kyverno/license/license      # expect: nctl-license-v1

# 3. state agrees
kubectl -n kyverno get cm kyverno-license-state -o jsonpath='{.data.lastState}'
```

Expect `Licensed`. If it says `FreeTier`, the file never reached the container —
go to §7.

---

## 5. Checking a customer's licensing state

Three places, in order of how quickly they answer the question.

### The state ConfigMap — fastest

```bash
kubectl -n kyverno get cm kyverno-license-state -o jsonpath='{.data}' | jq
```

```json
{
  "schemaVersion": "1",
  "firstSeen": "2026-08-01T09:12:44Z",
  "lastSeen":  "2026-08-14T11:02:44Z",
  "lastState": "Licensed",
  "hmac": "..."
}
```

| Field | Meaning |
|---|---|
| `lastState` | Current state, from the table in §3 |
| `firstSeen` | When this cluster first ran N4K — anchors the free trial |
| `lastSeen` | Last evaluation |
| `graceStartedAt` | Present only when a paid grace window is open — when non-compliance was first detected |
| `compliantSince` | Present when the customer has returned to compliance but grace hasn't cleared yet (needs 24h) |
| `hmac` | Integrity checksum. A mismatch is logged, not fatal. |

### Metrics — for entitlement vs usage

```bash
kubectl -n kyverno port-forward deploy/kyverno-admission-controller 8000:8000
curl -s localhost:8000/metrics | grep kyverno_nirmata_license
```

| Metric | Read it as |
|---|---|
| `..._state{state="X"} 1` | Current state (one series per state, one of them is 1) |
| `..._policies_used` / `..._policies_allowed` | Usage vs cap. Cap of `-1` means unlimited |
| `..._node_units_used` / `..._node_units_allowed` | Same, for node units |
| `..._days_remaining` | Days left in the current window (license, trial, or grace) |
| `..._over_limit{resource="policies"}` | `1` when over that cap |
| `..._info{customer,license_type,license_id,origin,commercial,invalid}` | Which license is actually loaded |
| `..._valid` | `1` in `Licensed`, `Warning`, `FreeTier` |
| `..._evaluation_errors` | Something is wrong with the license or its file |

`_info` is the one to ask for when a customer insists their license is installed —
it shows the `license_id` N4K actually loaded, `origin="file"` vs `"embedded"`, and
whether it was rejected as `invalid`.

### Events and logs — for the why

```bash
kubectl -n kyverno get events --field-selector involvedObject.name=kyverno-admission-controller
kubectl -n kyverno logs deploy/kyverno-admission-controller | grep -i licen
```

Events are on the admission controller Deployment:

| Reason | Type | Fires when |
|---|---|---|
| `LicenseStatus` | Normal | Moved into a healthy state |
| `LicenseWarning` | Warning | Moved into a state needing attention |
| `LicenseLimitExceeded` | Warning | Over a cap; repeats daily while it lasts |

### One-shot collection for a ticket

```bash
NS=kyverno
kubectl -n $NS get cm kyverno-license-state -o yaml
kubectl -n $NS get deploy kyverno-admission-controller -o jsonpath='{.spec.template.spec.containers[0].args}'
kubectl -n $NS logs deploy/kyverno-admission-controller --tail=500 | grep -i licen
kubectl -n $NS get events --field-selector involvedObject.name=kyverno-admission-controller
kubectl get clusterpolicy,policy,vpol,mpol,gpol,ivpol,dpol -A --no-headers | wc -l
kubectl get nodes -o custom-columns=NAME:.metadata.name,MEM:.status.capacity.memory
```

---

## 6. What the customer sees when the cap bites

```
Error from server: admission webhook "license.kyverno.svc" denied the request:
Nirmata license policy limit reached: 10 policies in use, 10 permitted
(state=FreeTier). Obtain or renew a license to create additional policies;
existing policies continue to enforce
```

The message names the count, the cap, and the state, so it usually diagnoses
itself. Options for the customer, in order:

1. **Buy capacity** — a new license with a higher `max_policies`, applied per §4.
2. **Delete unused policies** — DELETE is never blocked. Under the cap, creates work
   again at the next admission request.
3. **Consolidate** — several rules in one policy count as one policy.

There is no override, bypass flag, or "disable licensing" setting. That is
deliberate; do not promise one.

---

## 7. Triage

| Customer says | Most likely cause | Check | Fix |
|---|---|---|---|
| "We installed our license but still see a cap of 10" | The file never reached the container | `_info` metric shows `origin="embedded"`; logs say `commercial license file is missing or empty` | Confirm the Secret exists in the release namespace, and that `--licensePath` matches the actual mount path (§4) |
| "We set `licensePath` and it still doesn't work" | Flag set, nothing mounted | `kubectl exec ... head -1 <path>` fails | Use `license.create` or `license.existingSecret` instead, or add the volume + mount (§4 Option C) |
| "State says `Grace`, we just renewed" | Renewal not picked up yet | `_info` shows the old `license_id` | Wait for the next evaluation (≤15 min) or `rollout restart` |
| "State says `Grace` and we're inside our dates" | Over a capacity limit, not expiry | `_over_limit` metric; `_policies_used` vs `_allowed` | Sell capacity or reduce usage. Nothing is blocked meanwhile |
| "We're compliant again but still shows `graceStartedAt`" | Grace clears only after 24h of sustained compliance | `compliantSince` is set and recent | Expected. It clears on its own |
| "Trial restarted / we lost our trial days" | The `kyverno-license-state` ConfigMap was deleted or recreated | Compare `firstSeen` to install date | Keep `config.licenseState.preserve: true` (the default). Do not delete that ConfigMap |
| "Licensing broke our cluster / blocked our pods" | Not licensing | Nothing in licensing gates workload admission | Look at the customer's own Kyverno policies (`failureAction: Enforce`) or the policy webhooks |
| "Can we turn licensing off?" | — | — | No. No flag, no Helm value, and the gate webhook is reconciled back if deleted |
| "Our license is expired but everything works" | Working as designed | State is `Grace`, `_days_remaining` counting down | Renewal conversation — quote the days remaining |
| "Metrics show `_policies_allowed -1`" | The license file is present but **invalid** | `_info` shows `invalid="true"`; logs say `present but invalid` | Reissue the license. Caps are unlimited but the cluster is in `Grace` → `Degraded` |
| "Some policy kinds aren't counted" | Missing RBAC after a partial upgrade | Log: `some Kyverno policy kinds could not be listed` | Complete the Helm upgrade so the licensing RBAC rule is applied |

---

## 8. FAQ

**Does N4K phone home?**
No. Verification is a signature check against a public key baked into the image.
Works fully air-gapped.

**What happens if they delete the license Secret?**
Pods keep running — no restart, no crash. N4K falls back to the free tier at the
next evaluation, so the cap becomes 10 and the state becomes `FreeTier` or
`FreeExpired`. Re-applying the Secret restores it.

**Do we need a restart to apply a license?**
No. Picked up within 15 minutes. Restart only if they want it now.

**Does the trial restart on reinstall?**
No, as long as `config.licenseState.preserve` stays `true` (the default) — the
state ConfigMap survives `helm uninstall`. Deleting it manually does reset the
trial.

**Does licensing work in HA (multiple replicas)?**
Yes. Only the leader writes state and counts nodes. During a large burst of policy
creates spread across replicas, the cap can be briefly overshot by a policy or two;
the next evaluation reports it and further creates are refused. This is intentional
— the alternative would add a failure mode to the admission path.

**Are Kyverno-managed or built-in policies counted?**
Every policy object in the counted kinds is counted, including ones the customer
installed from the `kyverno-policies` chart. Point them at that chart if their
count is unexpectedly high.

**Can they see this in Prometheus/Grafana?**
Yes — all metrics are on the standard Kyverno metrics endpoint, prefixed
`kyverno_nirmata_license_`.

**Is the state ConfigMap tamper-proof?**
No, and we don't claim it is. The checksum catches partial writes and hand edits.
Anyone with edit rights on the namespace can change it. It's bookkeeping, not DRM.

---

## 9. Before escalating to engineering

Attach:

1. The output of the collection block in §5.
2. The `license_id` from the `_info` metric (not the license file — it's signed
   customer data; the ID is enough to identify it).
3. Chart version and N4K image tag.
4. What the customer expected vs what they saw, including the exact error text.

Escalate immediately if: pods are crash-looping with licensing in the logs, or
workload admission is being refused with a licensing message. Neither should be
possible — both would be bugs, not configuration.

---

## 10. Reference

### Counted policy kinds

`kyverno.io`: `clusterpolicies`, `policies`

`policies.kyverno.io`: `validatingpolicies`, `namespacedvalidatingpolicies`,
`mutatingpolicies`, `namespacedmutatingpolicies`, `generatingpolicies`,
`namespacedgeneratingpolicies`, `imagevalidatingpolicies`,
`namespacedimagevalidatingpolicies`, `deletingpolicies`, `namespaceddeletingpolicies`

### Helm values

| Value | Default | Purpose |
|---|---|---|
| `license.create` | `false` | Have the chart create the license Secret from `license.value` |
| `license.value` | — | The license file contents. Required with `create` |
| `license.existingSecret` | — | Use a Secret the customer already made |
| `license.name` / `license.key` | `<release>-license` / `license` | Secret name and key |
| `features.licensing.licensePath` | auto | Where N4K reads the license inside the container. Only set for custom mounts |
| `features.licensing.evaluationInterval` | `15m` | How often state is re-evaluated. Clamped to `1m`–`1h` |
| `features.licensing.warnRatio` | `0.9` | Capacity fraction that trips `Warning` |
| `config.licenseState.preserve` | `true` | Keep the state ConfigMap on uninstall so the trial isn't reset |

### Objects licensing creates

| Object | Namespace | Purpose |
|---|---|---|
| `kyverno-license-state` (ConfigMap) | release ns | Trial and grace bookkeeping |
| `kyverno-license` (Secret) | release ns | The license file, when `license.create=true` |
| `kyverno-license-validating-webhook-cfg` | cluster | The policy-CREATE cap gate. `failurePolicy: Ignore`, CREATE only |

### Defaults when a license omits them

Trial 15 days · grace 30 days · expiry warning 30 days · capacity warning at 90%.
