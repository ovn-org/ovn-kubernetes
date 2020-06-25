E2E_REPORT_DIR:=$(CURDIR)/_artifacts
JOB_NAME?="$@"

.PHONY: install-kind
install-kind:
	./scripts/install-kind.sh

.PHONY: shard-%
shard-%:
	E2E_REPORT_DIR=$(E2E_REPORT_DIR) E2E_REPORT_PREFIX=$(JOB_NAME)_ ./scripts/e2e-kind.sh $@ $(WHAT)

.PHONY: control-plane
control-plane:
	E2E_REPORT_DIR=$(E2E_REPORT_DIR) E2E_REPORT_PREFIX=$(JOB_NAME)_ ./scripts/e2e-cp.sh
