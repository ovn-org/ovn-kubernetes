.PHONY: install-kind
install-kind:
	./scripts/install-kind.sh

.PHONY: e2e-kind-ovn-shard-%
e2e-kind-ovn-shard-%:
	./scripts/e2e-kind.sh $@

.PHONY: e2e-control-plane
e2e-control-plane:
	./scripts/e2e-cp.sh
