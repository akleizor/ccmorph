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

.PHONY: help test test-zsh test-py check-version release clean

help:
	@echo "ccmorph $(VERSION)"
	@echo "  make test           run every suite"
	@echo "  make check-version  version, changelog and tag must agree"
	@echo "  make release        verify, then build $(TARBALL) + .sha256"

test: test-zsh test-py

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
release: check-version test
	@mkdir -p $(DIST)
	git archive --format=tar.gz --prefix=ccmorph-$(TAG)/ -o $(TARBALL) $(TAG)
	@cd $(DIST) && shasum -a 256 ccmorph-$(TAG).tar.gz > ccmorph-$(TAG).tar.gz.sha256
	@echo
	@echo "$(TARBALL)"
	@cat $(TARBALL).sha256
	@echo
	@echo "Verify the archive carries no untracked state:"
	@echo "  tar tzf $(TARBALL) | grep -E '\\.conf$$|^.*/(active|local\\.sh|model-registry)$$' && echo LEAK || echo clean"

clean:
	rm -rf $(DIST)
