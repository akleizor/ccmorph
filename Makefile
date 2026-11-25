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

.PHONY: help shim test test-go gate gate-launch gate-install gate-configure gate-py check-version dist release clean

help:
	@echo "ccmorph $(VERSION)"
	@echo "  make shim           build libexec/ccmorph-core (needs Go)"
	@echo "  make test           run every suite"
	@echo "  make check-version  version, changelog and tag must agree"
	@echo "  make release        verify, then build $(TARBALL) + the stable ccmorph.tar.gz"

# The compiled helpers. Tests and a checkout's install.sh use these binaries.
# macOS keys privacy grants to an ad-hoc binary's hash, so each build asks again;
# a real identity (CCMORPH_CODESIGN_IDENTITY) keeps them.
# The core is built and signed in a private directory inside libexec/, then
# renamed into place only after both steps succeed, so libexec/ccmorph-core only
# ever names a finished, signed build: a build that fails, is killed or times out
# leaves the published core as it was, and nothing reads a half-written or
# unsigned file (#714). Same directory, so the rename is atomic; it also gives
# the new core its own inode, never the bytes of a copy still running.
# A sandbox that refuses new names in the checkout — the landing gate's runner
# answers `mkdir libexec/.shim-build.<pid>`, and the same name in the checkout
# root, with "Operation not permitted" — still gets the same build: the staging
# name is tried in libexec/ first (main's, unchanged), then without the leading
# dot, then in the checkout root, then under ${TMPDIR} (made with mkdir -p, so
# a temp root that does not exist yet works), then as a plain file named
# ccmorph-core. Every candidate is on libexec's filesystem (device numbers
# compared, so a cross-filesystem `mv` — a copy, not a rename — is never used)
# and the publish is one atomic rename, never a write in place; when no staging
# name can be made, or the rename is refused, the build fails and the published
# core is left exactly as it was. Cleanup removes only what this run made: a
# staged directory it created, or the plain ccmorph-core file it created with
# noclobber (so a file already at that name is left alone and the build says it
# has no staging name), and never a directory that was already there — the
# caller's ${TMPDIR} is not this build's to remove.
# An identity this Mac no longer holds (a stale CCMORPH_CODESIGN_IDENTITY in
# the environment) is named and skipped rather than failing the build: Go's
# darwin linker already gives the core an ad-hoc signature, and a build that
# cannot run is a worse answer than the grant-less one.
shim:
	@ident="$(CCMORPH_CODESIGN_IDENTITY)"; \
	sign() { [ -z "$$ident" ] && return 0; \
	if [ "$$(command -v codesign)" = /usr/bin/codesign ] && \
	   ! security find-identity -v -p codesigning 2>/dev/null | grep -qF "$$ident"; then \
	  echo "shim: no codesigning identity $$ident on this Mac; leaving the linker's ad-hoc signature" >&2; return 0; fi; \
	codesign -f -s "$$ident" -i ccmorph-core "$$1"; }; \
	buildit() { go build -trimpath -o "$$1" ./cmd/ccmorph-core && sign "$$1"; }; \
	dev() { stat -c %d "$$1" 2>/dev/null || stat -f %d "$$1" 2>/dev/null; }; \
	same() { [ -n "$$(dev "$$1")" ] && [ "$$(dev "$$1")" = "$$(dev libexec)" ]; }; \
	stage=; made=; \
	for d in libexec/.shim-build.$$$$ libexec/shim-build.$$$$ .shim-build.$$$$ shim-build.$$$$ "$${TMPDIR:-/tmp}/ccmorph-shim.$$$$"; do \
	  rm -rf "$$d"; \
	  if mkdir -p "$$d" 2>/dev/null && same "$$d"; then made="$$d"; stage="$$d/ccmorph-core"; break; fi; \
	  rm -rf "$$d"; \
	done; \
	if [ -z "$$stage" ]; then \
	  for f in ccmorph-core "$${TMPDIR:-/tmp}/ccmorph-core"; do \
	    if ( set -C; : > "$$f" ) 2>/dev/null; then \
	      if same "$$f"; then stage="$$f"; break; fi; \
	      rm -f "$$f"; \
	    fi; \
	  done; \
	fi; \
	if [ -z "$$stage" ]; then \
	  echo "shim: no staging name on libexec's filesystem can be made; libexec/ccmorph-core is untouched" >&2; exit 1; \
	fi; \
	buildit "$$stage" && mv -f "$$stage" libexec/ccmorph-core; \
	rc=$$?; rm -f "$$stage"; if [ -n "$$made" ]; then rm -rf "$$made"; fi; exit $$rc

test: gate

# The suite runs a second time under umask 077, which catches mode-vs-umask
# fixture tests by name: os.WriteFile filters the mode it asks for through the
# host umask, so a test that writes 0644 and asserts it is born 0600 on a 077
# host. -count=1 keeps the cached 022 result from answering for it (#704).
# Env is a tool too, so the suite runs once more under NO_PROXY='*', the
# hostile shell of every machine that carries proxy exemptions: a test whose
# subject routes by env must not quietly assert what a scrubbed shell produced
# (#774, REVIEW.md). -count=1 for the same reason as above.
test-go:
	go vet ./...
	go test ./...
	umask 077 && go test -count=1 ./...
	NO_PROXY='*' no_proxy='*' go test -count=1 ./... || { echo "red under a hostile proxy env (NO_PROXY='*'): env is a tool too — a test that reads the operator's proxy env is the same failure as a test that sees the operator's bridge binary; fix it with t.Setenv in the test, not by scrubbing this line (#774, .github/REVIEW.md)"; exit 1; }

# The Hive's gate on every landing, and what `make test` runs. Each suite works in
# its own scratch homes and mostly waits on process spawns, so they overlap: 185 s
# one after another, about a minute at once on a quiet host. Never chain them.
gate: shim
	@$(MAKE) -j5 gate-launch gate-install gate-configure gate-py test-go

gate-launch:
	@zsh tests/ccmorph_test.zsh

gate-install:
	@zsh tests/install_test.zsh

gate-configure:
	@zsh tests/cli_configure_test.zsh

gate-py:
	@CCMORPH_LIB_DIR="$$PWD/lib" uv run python -m pytest tests/ -q

# Three places state the version and nothing checked they agree: the assignment
# the installer greps, the changelog a reader trusts, and the tag a package
# manager resolves. A release that disagrees with itself is how v1.5.1 came to
# have a changelog entry and no tag, invisible to anything resolving by tag.
# "Newest entry" is a fact about content, not about line numbers: the live
# Unreleased block sits above every release heading and grows with every
# landing, so the gate skips the first heading only when it is exactly
# `## Unreleased`; the next one must be TAG's. The
# `head -20` window this replaced held zero slack once v0.5.13 landed, and the
# next Unreleased bullet would have blocked `make release` (#610).
check-version:
	@test -n "$(VERSION)" || { echo "no _CCMORPH_VERSION in lib/ccmorph.sh"; exit 1; }
	@grep -q '^## $(TAG) ' CHANGELOG.md || { \
	  echo "CHANGELOG.md has no '## $(TAG)' section"; exit 1; }
	@awk '/^## /{ if (!n++ && $$0 == "## Unreleased") next; print; exit }' CHANGELOG.md | \
	  grep -q '^## $(TAG) ' || { \
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
# The tarball also carries one ccmorph-core per platform, built from the same
# tag export (never the working tree), so a user's install needs no Go.
PLATFORMS := darwin/arm64 darwin/amd64 linux/arm64 linux/amd64
STAGE := $(DIST)/stage/ccmorph-$(TAG)

dist:
	@rm -rf $(DIST)/stage && mkdir -p $(STAGE)
	git archive --format=tar $(TAG) | tar -x -C $(STAGE)
	@pids=""; fail=0; for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  (cd $(STAGE) && CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
	    go build -trimpath -o libexec/ccmorph-core-$$os-$$arch ./cmd/ccmorph-core) \
	    > $(DIST)/build-$$os-$$arch.log 2>&1 & \
	  pids="$$pids $$!"; \
	done; \
	for pid in $$pids; do wait $$pid || fail=1; done; \
	if [ $$fail -ne 0 ]; then cat $(DIST)/build-*.log; exit 1; fi; \
	rm -f $(DIST)/build-*.log
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

