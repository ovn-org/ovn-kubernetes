.PHONY: clean check_prep check

DOCKER ?= $(shell which docker)
IMAGE_NAME="goovn:test"

.PHONY=check
check: check_prep
	$(DOCKER) run -e "OVN_SRCDIR=/src/" -v $$PWD:/root/workspace -w /root/workspace -it $(IMAGE_NAME) .travis/test_run.sh

.PHONY=check_prep
check_prep:
	@$(DOCKER) inspect $(IMAGE_NAME) 2>&1 >/dev/null || \
	    $(DOCKER) build -t $(IMAGE_NAME) . ;

.PHONY=clean
clean:
	@docker rmi -f $(IMAGE_NAME)

