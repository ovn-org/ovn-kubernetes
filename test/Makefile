.PHONY: install-kind
install-kind:
	./scripts/install-kind.sh

.PHONY: shard-%
shard-%:
	./scripts/e2e-kind.sh $@ $(WHAT) 

.PHONY: control-plane
control-plane:
	./scripts/e2e-cp.sh
