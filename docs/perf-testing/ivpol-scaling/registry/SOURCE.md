# Registry facts this rig depends on, measured rather than assumed

Two questions gate every bundle-format (keyless / OCI referrers) cell. Both were
settled empirically on 2026-10-19 against the live rig, because both had been
answered wrongly by inference before.

## 1. distribution 3.1.2 has no Referrers API, but bundles still work

`GET /v2/<repo>/referrers/<digest>` returns `404 page not found` as plain text —
a Go mux "route not registered" 404, not a registry JSON error. The route does
not exist in distribution 3.x at all. It is unmerged PR
[#4828](https://github.com/distribution/distribution/pull/4828), milestone 3.2.0.

The earlier comment in `setup-registry.sh` concluded from this that the bundle
format could not be used here. That conclusion was wrong, and it cost the
benchmark its keyless leg.

**What actually happens.** go-containerregistry sets
`defaultReferrersTagFallback = true` (`pkg/v1/remote/options.go:113`) and nothing
in Kyverno, the kyverno SDK, or cosign ever calls `WithReferrersTagFallback` to
turn it off. So on a 404 the client falls back to the OCI dist-spec 1.1 referrers
tag schema: `GET /v2/<repo>/manifests/sha256-<hex>`.

**And cosign writes that tag.** Verified directly:

```sh
COSIGN_EXPERIMENTAL=1 cosign sign --yes --tlog-upload=false \
  --use-signing-config=false --allow-insecure-registry \
  --registry-referrers-mode=oci-1-1 --new-bundle-format=true \
  --key cosign.key 127.0.0.1:5997/bench/probe-bundle:v1
```

leaves the repository holding both `v1` and
`sha256-4dc82e8f...`, and that tag is an OCI index whose one manifest carries
`artifactType: application/vnd.dev.sigstore.bundle.v0.3+json`. Reading it back
offline succeeds:

```sh
cosign verify --insecure-ignore-tlog=true --allow-insecure-registry \
  --key cosign.pub --new-bundle-format=true 127.0.0.1:5997/bench/probe-bundle:v1
```

`--registry-referrers-mode=oci-1-1` is refused unless `COSIGN_EXPERIMENTAL=1` is
set, which is why an earlier attempt looked like the mode did not exist.

**Why the rig still moves to zot.** Not because distribution cannot carry
bundles, but because the fallback costs an extra round trip per attestor per
image: a 404 probe followed by the fallback-tag GET, where a referrers-capable
registry answers in one. That extra request would sit inside the measured `R`
and be attributed to the code path rather than to the registry. zot serves
referrers natively, so `R` is clean.

The distribution path is kept behind `REGISTRY_FLAVOR=distribution`, because
"what does a registry without the Referrers API cost you" is itself a result
worth one cell, and it is invisible to an operator.

## 2. Bundles must be key-signed here, not certificate-signed

The bundle sets were first built cert-signed, to mirror `bench/signed-cert` and
isolate the signature format against it. That does not work, and the failure is
worth recording because it looks like a registry problem and is not one.

`cosign verify --new-bundle-format=true --cert-chain <ca>` is refused outright
by cosign v3.1.3:

```
Error: unsupported: certificate chain must be in bundle and may not be
provided using --certificate-chain when using --new-bundle-format
```

Kyverno's in-process path refuses for the same underlying reason. `checkOptions`
sets `opts.SigVerifier` from a certificate only when the policy supplies the
leaf `cert:` (`opts.go:229-240`); with `certChain` alone it stays nil. A
certificate attestor with `insecureIgnoreTlog` also takes `skipSigstoreInfra`,
so `opts.TrustedMaterial` is nil as well (`opts.go:116-146`). cosign's
`verificationOptions` then fails before any registry work:

```go
if co.TrustedMaterial == nil && co.SigVerifier == nil {
    return nil, nil, nil, fmt.Errorf("a trusted root is required for identity-based verification")
}
```

A key attestor sets `SigVerifier` from the public key (`opts.go:215`) and still
takes the offline fast path, so key-plus-bundle verifies with no Sigstore
infrastructure. Confirmed end to end on this rig before the sets were rebuilt:
signed with `--key`, verified with `--key --new-bundle-format=true`, offline,
exit 0.

The bundle sets are therefore signed with keys A and B, the same keys as
`bench/signed` and `bench/signed-b`. That is a better comparison than the cert
version would have been: `bench/signed` and `bench/signed-bundle` now differ in
signature format and nothing else at all.

## 3. Certificate expiry is not a hazard on this rig

With `insecureIgnoreTlog: true` and no timestamp, cosign verifies against
`WithCurrentTime()`, so a signing certificate outside its validity window is
rejected. For Fulcio-issued certificates that window is about ten minutes, which
would make a pre-signed image set unusable for a sweep lasting hours.

It does not apply here. `mint_leaf_cert` issues with `-days 3650`, and the
certificates currently in `keys/` run from 2026-10-19 to 2036-10-16. A sweep
cannot outlive them.

This is worth stating on any slide that compares this rig to real keyless: real
Fulcio certificates are short-lived, which is precisely why a transparency-log
entry or an RFC3161 timestamp is load-bearing in production and is not here. The
rig measures the registry and verification cost, not the certificate lifecycle.
