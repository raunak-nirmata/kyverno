# Namespaced registry credential admission

Exercise all four policy-controlled secret reference surfaces through deployed
admission webhooks. Bare and explicit local references must be accepted;
foreign and installation namespace references must fail with the scope error.
No image pull or external registry is needed because only policies are created.
The attestor field uses the current API spelling `source.PullSecrets`.

Run with a patched Kyverno deployment:

```sh
chainsaw test --test-dir test/conformance/chainsaw/verify-images/namespaced-credential-scope
```

Runtime behavior for pre-existing stored policies is additionally exercised in
`test/integration/ivpol/credential_scope_test.go` and the legacy image handler
regressions.
