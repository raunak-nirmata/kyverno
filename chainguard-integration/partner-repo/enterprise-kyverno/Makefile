.PHONY: test
test: ## Run integration tests for all N4K components
test: test-kyverno
test: test-kyvernopre
test: test-background-controller
test: test-cleanup-controller
test: test-reports-controller
test: test-kubectl-kyverno
test: test-readiness-checker

.PHONY: test-kyverno
test-kyverno:
	@cd kyverno && make test

.PHONY: test-kyvernopre
test-kyvernopre:
	@cd kyvernopre && make test

.PHONY: test-background-controller
test-background-controller:
	@cd background-controller && make test

.PHONY: test-cleanup-controller
test-cleanup-controller:
	@cd cleanup-controller && make test

.PHONY: test-reports-controller
test-reports-controller:
	@cd reports-controller && make test

.PHONY: test-kubectl-kyverno
test-kubectl-kyverno:
	@cd kubectl-kyverno && make test

.PHONY: test-readiness-checker
test-readiness-checker:
	@cd readiness-checker && make test
