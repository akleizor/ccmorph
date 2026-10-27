# ccmorph — test and release.
#
# `make release` exists because a release tarball must never be built from a
# working tree. This repository's own working directory doubles as the runtime
# install for its author, so `tar` over it would package live credentials.
# `git archive` only ever sees tracked files at a named ref, which makes that
# mistake unrepresentable rather than merely discouraged.

SHELL := /bin/sh
VERSION := $(shell grep -m1 '^_CCMORPH_VERSION=' lib/ccmorph.sh | cut -d'"' -f2)
TAG := v$(VERSION)
DIST := dist
TARBALL := $(DIST)/ccmorph-$(TAG).tar.gz

.PHONY: help shim test test-zsh test-py test-go check-version dist release clean

help:
	@echo "ccmorph $(VERSION)"
	@echo "  make shim           build libexec/gateway-shim (needs Go)"
	@echo "  make test           run every suite"
	@echo "  make check-version  version, changelog and tag must agree"
	@echo "  make release        verify, then build $(TARBALL) + the stable ccmorph.tar.gz"

# The one compiled helper. Tests and a checkout's install.sh use this binary.
shim:
	go build -trimpath -o libexec/gateway-shim ./cmd/gateway-shim

test: shim test-go test-zsh test-py

test-go:
	go vet ./...
	go test ./...

test-zsh:
	zsh tests/ccmorph_test.zsh
	zsh tests/install_test.zsh

test-py:
	python3 -m pytest tests/ -q

# Three places state the version and nothing checked they agree: the assignment
# the installer greps, the changelog a reader trusts, and the tag a package
# manager resolves. A release that disagrees with itself is how v1.5.1 came to
# have a changelog entry and no tag, invisible to anything resolving by tag.
check-version:
	@test -n "$(VERSION)" || { echo "no _CCMORPH_VERSION in lib/ccmorph.sh"; exit 1; }
	@grep -q '^## $(TAG) ' CHANGELOG.md || { \
	  echo "CHANGELOG.md has no '## $(TAG)' section"; exit 1; }
	@head -20 CHANGELOG.md | grep -q '^## $(TAG) ' || { \
	  echo "'## $(TAG)' is not the newest CHANGELOG entry"; exit 1; }
	@git rev-parse -q --verify "refs/tags/$(TAG)" >/dev/null || { \
	  echo "tag $(TAG) does not exist — create it before releasing:"; \
	  echo "  git tag $(TAG)"; exit 1; }
	@echo "version $(VERSION): source, changelog and tag agree"

# Built from the TAG, never from the working tree — see the note at the top.
# TWO tarballs, one archive: the tag-named file, and a byte-identical copy under
# the stable name ccmorph.tar.gz. The one-liner installs
# releases/latest/download/ccmorph.tar.gz by default, so a release that attaches
# only the tag-named file leaves the default install downloading a 404; attach
# install.sh to the release too, since the one-liner fetches it from there.
# Split out of `release` because its own `test` prerequisite would recurse into
# the suite that tests this step.
# The tarball also carries one gateway-shim per platform, built from the same
# tag export (never the working tree), so a user's install needs no Go.
PLATFORMS := darwin/arm64 darwin/amd64 linux/arm64 linux/amd64
STAGE := $(DIST)/stage/ccmorph-$(TAG)

dist:
	@rm -rf $(DIST)/stage && mkdir -p $(STAGE)
	git archive --format=tar $(TAG) | tar -x -C $(STAGE)
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  (cd $(STAGE) && CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
	    go build -trimpath -o libexec/gateway-shim-$$os-$$arch ./cmd/gateway-shim) || exit 1; \
	done
	tar -czf $(TARBALL) -C $(DIST)/stage ccmorph-$(TAG)
	@rm -rf $(DIST)/stage
	cp $(TARBALL) $(DIST)/ccmorph.tar.gz
	@cd $(DIST) && shasum -a 256 ccmorph-$(TAG).tar.gz > ccmorph-$(TAG).tar.gz.sha256
	@echo
	@echo "$(TARBALL)"
	@echo "$(DIST)/ccmorph.tar.gz  <- the one-liner's default download"
	@cat $(TARBALL).sha256
	@echo
	@echo "Verify the archive carries no untracked state:"
	@echo "  tar tzf $(TARBALL) | grep -E '\\.conf$$|^.*/(active|local\\.sh|model-registry)$$' && echo LEAK || echo clean"

release: check-version test dist

clean:
	rm -rf $(DIST)

