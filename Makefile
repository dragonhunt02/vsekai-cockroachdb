# Copyright 2014 The Cockroach Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
# implied. See the License for the specific language governing
# permissions and limitations under the License.

# WARNING: This Makefile is not easily understood. If you're here looking for
# typical Make invocations to build the project and run tests, you'll be better
# served by running `make help`.
#
# Maintainers: the output of `make help` is automatically generated from the
# double-hash (##) comments throughout this Makefile. Please submit
# improvements!

ifneq (,$(findstring v3.,v$(MAKE_VERSION)))
$(info $(yellow)Warning: your version of `make` seems old; your build may fail!$(term-reset))
endif

# We need to define $(GO) early because it's needed for defs.mk.
GO      ?= go
# xgo is needed also for defs.mk.
override xgo := GOFLAGS= $(GO)

# defs.mk stores cached values of shell commands to avoid recomputing them on
# every Make invocation. This has a small but noticeable effect, especially on
# noop builds.
# This needs to be the first rule because we're including build/defs.mk
# first thing below, and Make needs to know how to build it.
.SECONDARY: build/defs.mk
build/defs.mk: Makefile build/defs.mk.sig
ifndef IGNORE_GOVERS
	@GOFLAGS= build/go-version-check.sh $(GO) || { echo "Disable this check with IGNORE_GOVERS=1." >&2; exit 1; }
endif
	@echo "macos-version = $$(sw_vers -productVersion 2>/dev/null | grep -oE '[0-9]+\.[0-9]+')" > $@.tmp
	@echo "GOEXE = $$($(xgo) env GOEXE)" >> $@.tmp
	@echo "NCPUS = $$({ getconf _NPROCESSORS_ONLN || sysctl -n hw.ncpu || nproc; } 2>/dev/null)" >> $@.tmp
	@echo "UNAME = $$(uname)" >> $@.tmp
	@echo "UNAME_O = $$(uname -o)" >> $@.tmp
	@echo "HOST_TRIPLE = $$($$($(GO) env CC) -dumpmachine)" >> $@.tmp
	@echo "GO_ENV_CC = $$(which $$($(GO) env CC))" >> $@.tmp
	@echo "GO_ENV_CXX = $$(which $$($(GO) env CXX))" >> $@.tmp
	@echo "GIT_DIR = $$(git rev-parse --git-dir 2>/dev/null)" >> $@.tmp
	@echo "have-defs = 1" >> $@.tmp
	@set -e; \
	if ! cmp -s $@.tmp $@; then \
	   mv -f $@.tmp $@; \
	   echo "Detected change in build system. Rebooting make." >&2; \
	else rm -f $@.tmp; fi

include build/defs.mk

# Nearly everything below this point needs to have the vendor directory ready
# for use and will depend on bin/.submodules-initialized. In order to
# ensure this is available before the first "include" directive depending
# on it, we'll have it listed first thing.
#
# Note how the actions for this rule are *not* using $(GIT_DIR) which
# is otherwise defined in defs.mk above. This is because submodules
# are used in the process of defining the .mk files included by the
# Makefile, so it is not yet defined by the time
# `.submodules-initialized` is needed during a fresh build after a
# checkout.
.SECONDARY: bin/.submodules-initialized
bin/.submodules-initialized:
	gitdir=$$(git rev-parse --git-dir 2>/dev/null || true); \
	if test -n "$$gitdir"; then \
	   git submodule update --init --recursive; \
	fi
	mkdir -p $(@D)
	touch $@

# If the user wants to persist customizations for some variables, they
# can do so by defining `customenv.mk` in their work tree.
-include customenv.mk

ifeq "$(findstring bench,$(MAKECMDGOALS))" "bench"
$(if $(TESTS),$(error TESTS cannot be specified with `make bench` (did you mean BENCHES?)))
else
$(if $(BENCHES),$(error BENCHES can only be specified with `make bench`))
endif

# Prevent invoking make with a specific test name without a constraining
# package.
ifneq "$(filter bench% test% stress%,$(MAKECMDGOALS))" ""
ifeq "$(PKG)" ""
$(if $(subst -,,$(TESTS)),$(error TESTS must be specified with PKG (e.g. PKG=./pkg/sql)))
$(if $(subst -,,$(BENCHES)),$(error BENCHES must be specified with PKG (e.g. PKG=./pkg/sql)))
endif
endif

TYPE :=
ifneq "$(TYPE)" ""
$(error Make no longer understands TYPE. Use 'build/builder.sh mkrelease $(subst release-,,$(TYPE))' instead)
endif

# dep-build is set to non-empty if the .d files should be included.
# This definition makes it empty when only the targets "help" and/or "clean"
# are specified.
build-with-dep-files := $(or $(if $(MAKECMDGOALS),,implicit-all),$(filter-out help clean,$(MAKECMDGOALS)))

## Which package to run tests against, e.g. "./pkg/foo".
PKG := ./pkg/...

## Tests to run for use with `make test`
TESTS := .

## Benchmarks to run for use with `make bench`.
BENCHES :=

## Space delimited list of logic test files to run, for make testlogic/testoptlogic.
FILES :=

## Name of a logic test configuration to run, for make testlogic/testoptlogic.
## (default: all configs. It's not possible yet to specify multiple configs in this way.)
TESTCONFIG :=

## Regex for matching logic test subtests. This is always matched after "FILES"
## if they are provided.
SUBTESTS :=

## Test timeout to use for the linter.
LINTTIMEOUT := 30m

## Test timeout to use for regular tests.
TESTTIMEOUT := 60m

## Test timeout to use for race tests.
RACETIMEOUT := 45m

## Test timeout to use for acceptance tests.
ACCEPTANCETIMEOUT := 30m

## Test timeout to use for benchmarks.
BENCHTIMEOUT := 5m

## Extra flags to pass to the go test runner, e.g. "-v --vmodule=raft=1"
TESTFLAGS :=

## Flags to pass to `go test` invocations that actually run tests, but not
## elsewhere. Used for the -json flag which we'll only want to pass
## selectively.  There's likely a better solution.
GOTESTFLAGS :=

## Extra flags to pass to `stress` during `make stress`.
STRESSFLAGS :=

## Cluster to use for `make roachprod-stress`
CLUSTER :=

## Verbose allows turning on verbose output from the cmake builds.
VERBOSE :=

## Indicate the base root directory where to install
DESTDIR :=

DUPLFLAGS    := -t 100
GOFLAGS      :=
TAGS         := $(if $(findstring SunOS,$(UNAME)),$(if $(findstring illumos,$(UNAME_O)),stdmalloc))
ARCHIVE      := cockroach.src.tgz
STARTFLAGS   := -s type=mem,size=1GiB --logtostderr
BUILDTARGET  := ./pkg/cmd/cockroach
SUFFIX       := $(GOEXE)
INSTALL      := install
prefix       := /usr/local
bindir       := $(prefix)/bin

# We always want to build from the vendor directory.
# Avoid reusing GOFLAGS as that is overwritten by various release processes.
GOMODVENDORFLAGS := -mod=vendor

help: ## Print help for targets with comments.
	@echo "Usage:"
	@echo "  make [target...] [VAR=foo VAR2=bar...]"
	@echo ""
	@echo "Useful commands:"
	@git grep -Ph '^[a-zA-Z._-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  $(cyan)%-30s$(term-reset) %s\n", $$1, $$2}'
	@echo ""
	@echo "Useful variables:"
	@awk 'BEGIN { RS = "" ; FS = "\n" } /^## /{split($$NF, a, ":="); printf "  $(cyan)%-30s$(term-reset)", a[1]; x=1; while ( x<NF ) { c = substr($$x, 4); printf "  %-30s", c; x++} print ""}' $(MAKEFILE_LIST) | sort
	@echo ""
	@echo "Typical usage:"
	@printf "  $(cyan)%s$(term-reset)\n    %s\n\n" \
		"make test" "Run all unit tests." \
		"make test PKG=./pkg/sql" "Run all unit tests in the ./pkg/sql package" \
		"make test PKG=./pkg/sql/parser TESTS=TestParse" "Run the TestParse test in the ./pkg/sql/parser package." \
		"make bench PKG=./pkg/sql/parser BENCHES=BenchmarkParse" "Run the BenchmarkParse benchmark in the ./pkg/sql/parser package." \
		"make testlogic" "Run all base and opt exec builder logic tests." \
		"make testoptlogic" "Run all opt exec builder SQL logic tests." \
		"make testbaselogic" "Run all the OSS SQL logic tests." \
		"make testlogic FILES='prepare|fk'" "Run the logic tests in the files named prepare and fk (the full path is not required)." \
		"make testlogic FILES=fk SUBTESTS='20042|20045'" "Run the logic tests within subtests 20042 and 20045 in the file named fk." \
		"make testlogic TESTCONFIG=local" "Run the logic tests for the cluster configuration 'local'." \
		"make fuzz" "Run all fuzz tests for 12m each (or whatever the default TESTTIMEOUT is)." \
		"make fuzz PKG=./pkg/sql/... TESTTIMEOUT=1m" "Run all fuzz tests under the sql directory for 1m each." \
		"make fuzz PKG=./pkg/sql/sem/tree TESTS=Decimal TESTTIMEOUT=1m" "Run the Decimal fuzz tests in the tree directory for 1m."

BUILDTYPE := development

# Build C/C++ with basic debugging information.
CFLAGS += -g1
CXXFLAGS += -g1
LDFLAGS ?=

# TODO(benesch): remove filter-outs below when golang/go#26144 and
# golang/go#16651, respectively, are fixed.
CGO_CFLAGS = $(filter-out -g%,$(CFLAGS))
CGO_CXXFLAGS = $(CXXFLAGS)
CGO_LDFLAGS = $(filter-out -static,$(LDFLAGS))
# certain time based fail if UTC isn't the default timezone
TZ=""

export CFLAGS CXXFLAGS LDFLAGS CGO_CFLAGS CGO_CXXFLAGS CGO_LDFLAGS TZ

# We intentionally use LINKFLAGS instead of the more traditional LDFLAGS
# because LDFLAGS has built-in semantics that don't make sense with the Go
# toolchain.
override LINKFLAGS = -X github.com/cockroachdb/cockroach/pkg/build.typ=$(BUILDTYPE) -extldflags "$(LDFLAGS)"

GOMODVENDORFLAGS ?= -mod=vendor
GOFLAGS ?=
TAR     ?= tar

# We install our vendored tools to a directory within this repository to avoid
# overwriting any user-installed binaries of the same name in the default GOBIN.
GO_INSTALL := GOBIN='$(abspath bin)' GOFLAGS= $(GO) install $(GOMODVENDORFLAGS)

# Prefer tools we've installed with go install and Yarn to those elsewhere on
# the PATH.
export PATH := $(abspath bin):$(PATH)

# HACK: Make has a fast path and a slow path for command execution,
# but the fast path uses the PATH variable from when make was started,
# not the one we set on the previous line. In order for the above
# line to have any effect, we must force make to always take the slow path.
# Setting the SHELL variable to a value other than the default (/bin/sh)
# is one way to do this globally.
# http://stackoverflow.com/questions/8941110/how-i-could-add-dir-to-path-in-makefile/13468229#13468229
#
# We also force the PWD environment variable to $(CURDIR), which ensures that
# any programs invoked by Make see a physical CWD without any symlinks. The Go
# toolchain does not support symlinks well (for one example, see
# https://github.com/golang/go/issues/24359). This may be fixed when GOPATH is
# deprecated, so revisit whether this workaround is necessary then.
export SHELL := env PWD=$(CURDIR) bash
ifeq ($(SHELL),)
$(error bash is required)
endif

# Invocation of any NodeJS script should be prefixed by NODE_RUN. See the
# comments within node-run.sh for rationale.
NODE_RUN := build/node-run.sh

# make-lazy converts a recursive variable, which is evaluated every time it's
# referenced, to a lazy variable, which is evaluated only the first time it's
# used. See: http://blog.jgc.org/2016/07/lazy-gnu-make-variables.html
override make-lazy = $(eval $1 = $$(eval $1 := $(value $1))$$($1))

# GNU tar and BSD tar both support transforming filenames according to a regular
# expression, but have different flags to do so.
TAR_XFORM_FLAG = $(shell $(TAR) --version | grep -q GNU && echo "--xform='flags=r;s'" || echo "-s")
$(call make-lazy,TAR_XFORM_FLAG)

# MAKE_TERMERR is set automatically in Make v4.1+, but macOS is still shipping
# v3.81.
MAKE_TERMERR ?= $(shell [[ -t 2 ]] && echo true)

# This is how you get a literal space into a Makefile.
space := $(eval) $(eval)

# Color support.
yellow = $(shell { tput setaf 3 || tput AF 3; } 2>/dev/null)
cyan = $(shell { tput setaf 6 || tput AF 6; } 2>/dev/null)
term-reset = $(shell { tput sgr0 || tput me; } 2>/dev/null)
$(call make-lazy,yellow)
$(call make-lazy,cyan)
$(call make-lazy,term-reset)

# Warn maintainers for if ccache is not found.
ifeq (, $(shell which ccache))
$(info $(yellow)Warning: 'ccache' not found, consider installing it for faster builds$(term-reset))
endif

# Warn maintainers if bazel is not found.
ifeq (, $(shell which bazel))
$(info $(yellow)Warning: 'bazel' not found (`brew install bazelisk` for macs)$(term-reset))
endif

# Tell Make to delete the target if its recipe fails. Otherwise, if a recipe
# modifies its target before failing, the target's timestamp will make it appear
# up-to-date on the next invocation of Make, even though it is likely corrupt.
# See: https://www.gnu.org/software/make/manual/html_node/Errors.html#Errors
.DELETE_ON_ERROR:

# Targets that name a real file that must be rebuilt on every Make invocation
# should depend on .ALWAYS_REBUILD. (.PHONY should only be used on targets that
# don't name a real file because .DELETE_ON_ERROR does not apply to .PHONY
# targets.)
.ALWAYS_REBUILD:
.PHONY: .ALWAYS_REBUILD


CLUSTER_UI_JS := pkg/ui/cluster-ui/dist/main.js

.SECONDARY: $(CLUSTER_UI_JS)
$(CLUSTER_UI_JS): $(shell find pkg/ui/workspaces/cluster-ui/src -type f | sed 's/ /\\ /g') pkg/ui/yarn.installed pkg/ui/workspaces/db-console/src/js/protos.d.ts | bin/.submodules-initialized
	$(NODE_RUN) -C pkg/ui/workspaces/cluster-ui yarn build

.SECONDARY: pkg/ui/yarn.installed
pkg/ui/yarn.installed: pkg/ui/package.json pkg/ui/yarn.lock | bin/.submodules-initialized
	@# Do not install optional dependencies as far as they are required for UI development only
	@# and should not be installed for production builds.
	@# Also some linux distributions (that are used as development env) don't support some of
	@# optional dependencies (i.e. cypress) so it is important to make these deps optional.
	$(NODE_RUN) -C pkg/ui yarn install --ignore-optional --offline
	@# We remove this broken dependency again in pkg/ui/webpack.config.js.
	@# See the comment there for details.
	rm -rf pkg/ui/node_modules/@types/node
	touch $@

vendor/modules.txt: go.mod go.sum
	$(GO) mod download
	$(GO) mod vendor
	$(GO_INSTALL) -v github.com/goware/modvendor
	modvendor -copy="**/*.c **/*.h **/*.proto" -include 'github.com/grpc-ecosystem/grpc-gateway/third_party/googleapis/google/api,github.com/grpc-ecosystem/grpc-gateway/third_party/googleapis/google/rpc,github.com/prometheus/client_model'
	for patch in patches/*.patch; do patch -p1 <$$patch; done

# Update the git hooks and install commands from dependencies whenever they
# change.
# These should be synced with `./pkg/cmd/import-tools/main.go`.
bin/.bootstrap: vendor/modules.txt
	@$(GO_INSTALL) -v \
		github.com/bufbuild/buf/cmd/buf \
		github.com/client9/misspell/cmd/misspell \
		github.com/cockroachdb/crlfmt \
		github.com/cockroachdb/gostdlib/cmd/gofmt \
		github.com/cockroachdb/gostdlib/x/tools/cmd/goimports \
		github.com/cockroachdb/stress \
		github.com/cockroachdb/tools/cmd/stringer \
		github.com/go-swagger/go-swagger/cmd/swagger \
		github.com/golang/mock/mockgen \
		github.com/goware/modvendor \
		github.com/grpc-ecosystem/grpc-gateway/protoc-gen-grpc-gateway \
		github.com/kevinburke/go-bindata/go-bindata \
		github.com/kisielk/errcheck \
		github.com/mattn/goveralls \
		github.com/mibk/dupl \
		github.com/mmatczuk/go_generics/cmd/go_generics \
		github.com/pseudomuto/protoc-gen-doc/cmd/protoc-gen-doc \
		github.com/wadey/gocovmerge \
		golang.org/x/perf/cmd/benchstat \
		golang.org/x/tools/cmd/goyacc \
		golang.org/x/tools/go/analysis/passes/shadow/cmd/shadow \
		honnef.co/go/tools/cmd/staticcheck
	touch $@

IGNORE_GOVERS :=

# The following section handles building our C/C++ dependencies. These are
# common because both the root Makefile and protobuf.mk have C dependencies.

host-is-macos := $(findstring Darwin,$(UNAME))
host-is-mingw := $(findstring MINGW,$(UNAME))

ifdef host-is-macos
# On macOS 10.11, XCode SDK v8.1 (and possibly others) indicate the presence of
# symbols that don't exist until macOS 10.12. Setting MACOSX_DEPLOYMENT_TARGET
# to the host machine's actual macOS version works around this. See:
# https://github.com/jemalloc/jemalloc/issues/494.
export MACOSX_DEPLOYMENT_TARGET ?= $(macos-version)
endif

# Cross-compilation occurs when you set TARGET_TRIPLE to something other than
# HOST_TRIPLE. You'll need to ensure the cross-compiling toolchain is on your
# path and override the rest of the variables that immediately follow as
# necessary. For an example, see build/builder/cmd/mkrelease, which sets these
# variables appropriately for the toolchains baked into the builder image.
TARGET_TRIPLE := $(HOST_TRIPLE)
XCMAKE_SYSTEM_NAME :=
XGOOS :=
XGOARCH :=
XCC := $(TARGET_TRIPLE)-cc
XCXX := $(TARGET_TRIPLE)-c++
EXTRA_XCMAKE_FLAGS :=
EXTRA_XCONFIGURE_FLAGS :=

ifneq ($(HOST_TRIPLE),$(TARGET_TRIPLE))
is-cross-compile := 1
endif

target-is-windows := $(findstring w64,$(TARGET_TRIPLE))
target-is-macos := $(findstring darwin,$(TARGET_TRIPLE))
target-is-linux := $(findstring linux,$(TARGET_TRIPLE))

# CMAKE_TARGET_MESSAGES=OFF prevents CMake from printing progress messages
# whenever a target is fully built to prevent spammy output from make when
# c-deps are all already built. Progress messages are still printed when actual
# compilation is being performed.
cmake-flags := -DCMAKE_TARGET_MESSAGES=OFF $(if $(host-is-mingw),-G 'MSYS Makefiles')
configure-flags :=

# Use xcmake-flags when invoking CMake on binaries for the target
# platform (i.e., the cross-compiled platform, if specified); use plain
# cmake-flags when invoking CMake on libraries/binaries for the host platform.
# Similarly for xconfigure-flags and configure-flags, and xgo and GO.
xcmake-flags := $(cmake-flags) $(EXTRA_XCMAKE_FLAGS)
xconfigure-flags := $(configure-flags) $(EXTRA_XCONFIGURE_FLAGS)

# If we're cross-compiling, inform Autotools and CMake.
ifdef is-cross-compile
xconfigure-flags += --host=$(TARGET_TRIPLE) CC=$(XCC) CXX=$(XCXX)
cmake-flags += -DCMAKE_C_COMPILER=$(GO_ENV_CC) -DCMAKE_CXX_COMPILER=$(GO_ENV_CXX)
xcmake-flags += -DCMAKE_SYSTEM_NAME=$(XCMAKE_SYSTEM_NAME) -DCMAKE_C_COMPILER=$(XCC) -DCMAKE_CXX_COMPILER=$(XCXX)
override xgo := GOFLAGS= GOOS=$(XGOOS) GOARCH=$(XGOARCH) CC=$(XCC) CXX=$(XCXX) $(xgo)
endif

C_DEPS_DIR := $(abspath c-deps)
JEMALLOC_SRC_DIR := $(C_DEPS_DIR)/jemalloc
GEOS_SRC_DIR     := $(C_DEPS_DIR)/geos
PROJ_SRC_DIR     := $(C_DEPS_DIR)/proj

# Derived build variants.
use-stdmalloc          := $(findstring stdmalloc,$(TAGS))

BUILD_DIR := $(CURDIR)/lib/build

# In MinGW, cgo flags don't handle Unix-style paths, so convert our base path to
# a Windows-style path.
#
# TODO(benesch): Figure out why. MinGW transparently converts Unix-style paths
# everywhere else.
ifdef host-is-mingw
BUILD_DIR := $(shell cygpath -m $(BUILD_DIR))
endif

JEMALLOC_DIR := $(BUILD_DIR)/jemalloc
GEOS_DIR     := $(BUILD_DIR)/geos
PROJ_DIR     := $(BUILD_DIR)/proj

LIBJEMALLOC := $(JEMALLOC_DIR)/lib/libjemalloc.a
LIBPROJ     := $(PROJ_DIR)/lib/libproj$(if $(target-is-windows),_4_9).a

DYN_LIB_DIR := lib
DYN_EXT     := so
ifdef target-is-macos
DYN_EXT     := dylib
endif
ifdef target-is-windows
DYN_EXT     := dll
endif

LIBGEOS     := $(DYN_LIB_DIR)/libgeos.$(DYN_EXT)

C_LIBS_COMMON = \
	$(if $(use-stdmalloc),,$(LIBJEMALLOC)) \
	$(LIBPROJ)
C_LIBS_SHORT = $(C_LIBS_COMMON)
C_LIBS_OSS = $(C_LIBS_COMMON)
C_LIBS_DYNAMIC = $(LIBGEOS)

# Go does not permit dashes in build tags. This is undocumented.
native-tag := $(subst -,_,$(TARGET_TRIPLE))$(if $(use-stdmalloc),_stdmalloc)

# In each package that uses cgo, we inject include and library search paths into
# files named zcgo_flags_{native-tag}.go. The logic for this is complicated so
# that Make-driven builds can cache the state of builds for multiple
# configurations at once, while still allowing the use of `go build` and `go
# test` for the configuration most recently built with Make.
#
# Building with Make always adds the `make` and {native-tag} tags to the build.
#
# Unsuffixed flags files (zcgo_flags.cgo) have the build constraint `!make` and
# are only compiled when invoking the Go toolchain directly on a package-- i.e.,
# when the `make` build tag is not specified. These files are rebuilt whenever
# the build signature changes (see build/defs.mk.sig), and so reflect the target
# triple that Make was most recently invoked with.
#
# Suffixed flags files (e.g. zcgo_flags_{native-tag}.go) have the build
# constraint `{native-tag}` and are built the first time a Make-driven build
# encounters a given native tag or when the build signature changes (see
# build/defs.mk.sig). These tags are unset when building with the Go toolchain
# directly, so these files are only compiled when building with Make.
CGO_PKGS := \
	pkg/cli \
	pkg/cli/clisqlshell \
	pkg/server/status \
	pkg/geo/geoproj \
	vendor/github.com/knz/go-libedit/unix
vendor/github.com/knz/go-libedit/unix-package := libedit_unix
CGO_UNSUFFIXED_FLAGS_FILES := $(addprefix ./,$(addsuffix /zcgo_flags.go,$(CGO_PKGS)))
CGO_SUFFIXED_FLAGS_FILES   := $(addprefix ./,$(addsuffix /zcgo_flags_$(native-tag).go,$(CGO_PKGS)))
BASE_CGO_FLAGS_FILES := $(CGO_UNSUFFIXED_FLAGS_FILES) $(CGO_SUFFIXED_FLAGS_FILES)
CGO_FLAGS_FILES := $(BASE_CGO_FLAGS_FILES) vendor/github.com/knz/go-libedit/unix/zcgo_flags_extra.go

$(BASE_CGO_FLAGS_FILES): Makefile build/defs.mk.sig vendor/modules.txt
	@echo "regenerating $@"
	@echo '// GENERATED FILE DO NOT EDIT' > $@
	@echo >> $@
	@echo '//go:build $(if $(findstring $(native-tag),$@),$(native-tag),!make)' >> $@
	@echo '// +build $(if $(findstring $(native-tag),$@),$(native-tag),!make)' >> $@
	@echo >> $@
	@echo 'package $(if $($(@D)-package),$($(@D)-package),$(notdir $(@D)))' >> $@
	@echo >> $@
	@echo '// #cgo CPPFLAGS: $(addprefix -I,$(JEMALLOC_DIR)/include)' >> $@
	@echo '// #cgo LDFLAGS: $(addprefix -L,$(JEMALLOC_DIR)/lib $(PROJ_DIR)/lib)' >> $@
	@echo 'import "C"' >> $@

vendor/github.com/knz/go-libedit/unix/zcgo_flags_extra.go: Makefile vendor/modules.txt
	@echo "regenerating $@"
	@echo '// GENERATED FILE DO NOT EDIT' > $@
	@echo >> $@
	@echo 'package $($(@D)-package)' >> $@
	@echo >> $@
	@echo '// #cgo !windows LDFLAGS: -ledit -lncurses' >> $@
	@echo 'import "C"' >> $@

# BUILD ARTIFACT CACHING
#
# We need to ensure that changes to a dependency's configure or CMake flags
# below cause the corresponding dependency to be rebuilt. It would be correct to
# have the dependencies list this file itself as a prerequisite, but *all*
# dependencies would be rebuilt, likely unnecessarily, whenever this file
# changed. Instead, we give each dependency its own marker file, DEP-rebuild, as
# a prerequisite.
#
# It's not important *what* goes in the marker file, so long as its contents
# change in the same commit as the configure flags. This causes Git to touch the
# marker file when switching between revisions that span the change. For
# simplicity, just sequentially bump the version number within.
#
# NB: the recipes below nuke *all* build artifacts when a dependency's configure
# flags change. In theory, we could rely on the dependency's build system to
# only rebuild the affected objects, but in practice dependencies on configure
# flags are not tracked correctly, and these stale artifacts can cause
# particularly hard-to-debug errors.
$(JEMALLOC_SRC_DIR)/configure.ac: | bin/.submodules-initialized

$(JEMALLOC_SRC_DIR)/configure: $(JEMALLOC_SRC_DIR)/configure.ac
	cd $(JEMALLOC_SRC_DIR) && autoconf

$(JEMALLOC_DIR)/Makefile: $(C_DEPS_DIR)/jemalloc-rebuild $(JEMALLOC_SRC_DIR)/configure
	rm -rf $(JEMALLOC_DIR)
	mkdir -p $(JEMALLOC_DIR)
	@# NOTE: If you change the configure flags below, bump the version in
	@# $(C_DEPS_DIR)/jemalloc-rebuild. See above for rationale.
	cd $(JEMALLOC_DIR) && $(JEMALLOC_SRC_DIR)/configure $(xconfigure-flags) --enable-prof

$(GEOS_DIR)/Makefile: $(C_DEPS_DIR)/geos-rebuild | bin/.submodules-initialized
	rm -rf $(GEOS_DIR)
	mkdir -p $(GEOS_DIR)
	@# NOTE: If you change the CMake flags below, bump the version in
	@# $(C_DEPS_DIR)/geos-rebuild. See above for rationale.
	cd $(GEOS_DIR) && \
	  cmake $(xcmake-flags) $(GEOS_SRC_DIR) -DCMAKE_BUILD_TYPE=Release -DCMAKE_C_FLAGS=-fPIC -DCMAKE_CXX_FLAGS="-fPIC -include cstddef -include cstdint"
	@# Copy geos/export.h to the capi include directory to avoid needing multiple include
	@# directories.
	mkdir $(GEOS_DIR)/capi/geos
	cp $(GEOS_SRC_DIR)/include/geos/export.h $(GEOS_DIR)/capi/geos

$(PROJ_DIR)/Makefile: $(C_DEPS_DIR)/proj-rebuild | bin/.submodules-initialized
	rm -rf $(PROJ_DIR)
	mkdir -p $(PROJ_DIR)
	cd $(PROJ_DIR) && cmake  $(xcmake-flags) $(PROJ_SRC_DIR) -DCMAKE_BUILD_TYPE=Release -DBUILD_LIBPROJ_SHARED=OFF

# Most of our C and C++ dependencies use Makefiles that are generated by CMake,
# which are rather slow, taking upwards of 500ms to determine that nothing has
# changed. The no-op case is the common case, as C and C++ code is modified
# rarely relative to Go code.
#
# So, for speed, we want to avoid invoking our C and C++ dependencies' build
# systems when nothing has changed. We apply a very coarse heuristic that works
# well in practice: if every file in a given library's source tree is older than
# the compiled library, then the compiled library must be up-to-date.
#
# Normally, you'd accomplish this in Make itself by declaring a prerequisite for
# every file in the library's source tree. For example, you'd have a rule like
# this for protoc:
#
#     $(PROTOC): $(PROTOC_DIR)/Makefile $(shell find c-deps/protobuf)
#       $(MAKE) -C $(PROTOC_DIR) protoc
#
# Note the prerequisite that shells out to the 'find' command. Unfortunately,
# this winds up being as slow as unconditionally invoking the child build
# system! The cost of repeated find invocations, one per command, adds up, plus
# Make needs to stat all of the resulting files, and it seems to do so
# sequentially.
#
# Instead, we unconditionally run the recipe for each C and C++ dependency, via
# .ALWAYS_REBUILD, but condition the execution of the dependency's build system
# on the output of uptodate, a Go binary of our own design. uptodate walks and
# stats the directory tree in parallel, and can make the up-to-date
# determination in under 20ms.

$(LIBJEMALLOC): $(JEMALLOC_DIR)/Makefile bin/uptodate .ALWAYS_REBUILD
	@uptodate $@ $(JEMALLOC_SRC_DIR) || $(MAKE) --no-print-directory -C $(JEMALLOC_DIR) build_lib_static

ifdef is-cross-compile
ifdef target-is-macos
geos_require_install_name_tool := 1
endif
ifdef target-is-linux
geos_require_patchelf := 1
endif
endif

# For dlopen to work with OSX from any location, we need the @rpath directory prefix.
# However, no matter what CMake flags I try, cross-compiling OSX does not output
# the correct rpath locations. As such, use the install-name-tool to do the work
# of setting the correct rpaths on OSX.
GEOS_NATIVE_LIB_DIR = $(GEOS_DIR)/$(if $(target-is-windows),bin,lib)
ifdef geos_require_install_name_tool
$(LIBGEOS): libgeos_inner .ALWAYS_REBUILD
	install_name_tool -id @rpath/libgeos.3.8.1.dylib lib/libgeos.dylib
	install_name_tool -id @rpath/libgeos_c.1.dylib lib/libgeos_c.dylib
	install_name_tool -change "$(GEOS_NATIVE_LIB_DIR)/libgeos.3.8.1.dylib" "@rpath/libgeos.3.8.1.dylib" lib/libgeos_c.dylib
else ifdef geos_require_patchelf
# We apply a similar fix for linux, allowing one to dlopen libgeos_c.so without
# dlopening libgeos.so. Setting the rpath in the CMakeLists.txt does not work
# for cross compilation.
$(LIBGEOS): libgeos_inner .ALWAYS_REBUILD
	patchelf --set-rpath '/usr/local/lib/cockroach/' lib/libgeos_c.so
	patchelf --set-soname libgeos.so lib/libgeos.so
	patchelf --replace-needed libgeos.so.3.8.1 libgeos.so lib/libgeos_c.so
else
$(LIBGEOS): libgeos_inner .ALWAYS_REBUILD
endif

libgeos_inner: $(GEOS_DIR)/Makefile bin/uptodate .ALWAYS_REBUILD
	@uptodate $(GEOS_NATIVE_LIB_DIR)/libgeos.$(DYN_EXT) $(GEOS_SRC_DIR) || $(MAKE) --no-print-directory -C $(GEOS_DIR) geos_c
	mkdir -p $(DYN_LIB_DIR)
	rm -f $(DYN_LIB_DIR)/lib{geos,geos_c}.$(DYN_EXT)
	cp $(GEOS_NATIVE_LIB_DIR)/lib{geos,geos_c}.$(DYN_EXT) $(DYN_LIB_DIR)

$(LIBPROJ): $(PROJ_DIR)/Makefile bin/uptodate .ALWAYS_REBUILD
	@uptodate $@ $(PROJ_SRC_DIR) || $(MAKE) --no-print-directory -C $(PROJ_DIR) proj

# Convenient names for maintainers. Not used by other targets in the Makefile.
.PHONY:  libjemalloc libgeos libproj
libjemalloc: $(LIBJEMALLOC)
libgeos:     $(LIBGEOS)
libproj:     $(LIBPROJ)

override TAGS += make $(native-tag)

# Some targets (protobuf) produce different results depending on the sort order;
# set LC_ALL so this is consistent across systems.
export LC_ALL=C

# defs.mk.sig attempts to capture common cases where defs.mk needs to be
# recomputed, like when compiling for a different platform or using a different
# Go binary. It is not intended to be perfect. Upgrading the compiler toolchain
# in place will go unnoticed, for example. Similar problems exist in all Make-
# based build systems and are not worth solving.
build/defs.mk.sig: sig = $(PATH):$(CURDIR):$(GO):$(CC):$(CXX):$(TARGET_TRIPLE):$(BUILDTYPE):$(IGNORE_GOVERS)
build/defs.mk.sig: .ALWAYS_REBUILD
	@echo '$(sig)' | cmp -s - $@ || echo '$(sig)' > $@

COCKROACHOSS   := ./cockroachoss$(SUFFIX)
COCKROACHSHORT := ./cockroachshort$(SUFFIX)
COCKROACHSQL   := ./cockroach-sql$(SUFFIX)

LOG_TARGETS = \
	pkg/util/log/severity/severity_generated.go \
	pkg/util/log/channel/channel_generated.go \
	pkg/util/log/eventpb/eventlog_channels_generated.go \
	pkg/util/log/eventpb/json_encode_generated.go \
	pkg/util/log/log_channels_generated.go

SQLPARSER_TARGETS = \
	pkg/sql/parser/sql.go \
	pkg/sql/parser/helpmap_test.go \
	pkg/sql/parser/help_messages.go \
	pkg/sql/lexbase/tokens.go \
	pkg/sql/lexbase/keywords.go \
	pkg/sql/lexbase/reserved_keywords.go

PROTOBUF_TARGETS := bin/.go_protobuf_sources bin/.gw_protobuf_sources

SWAGGER_TARGETS := \
  docs/generated/swagger/spec.json

DOCGEN_TARGETS := \
	bin/.docgen_bnfs \
	bin/.docgen_functions \
	docs/generated/redact_safe.md \
	bin/.docgen_http \
	bin/.docgen_logformats \
	docs/generated/logsinks.md \
	docs/generated/logging.md \
	docs/generated/eventlog.md

GENERATED_TARGETS = \
  pkg/roachprod/vm/aws/embedded.go \
  pkg/security/securitytest/embedded.go

EXECGEN_TARGETS = \
  pkg/col/coldata/vec.eg.go \
  pkg/sql/colconv/datum_to_vec.eg.go \
  pkg/sql/colconv/vec_to_datum.eg.go \
  pkg/sql/colexec/and_or_projection.eg.go \
  pkg/sql/colexec/hash_aggregator.eg.go \
  pkg/sql/colexec/is_null_ops.eg.go \
  pkg/sql/colexec/ordered_synchronizer.eg.go \
  pkg/sql/colexec/quicksort.eg.go \
  pkg/sql/colexec/rowtovec.eg.go \
  pkg/sql/colexec/select_in.eg.go \
  pkg/sql/colexec/sort.eg.go \
  pkg/sql/colexec/sorttopk.eg.go \
  pkg/sql/colexec/sort_partitioner.eg.go \
  pkg/sql/colexec/substring.eg.go \
  pkg/sql/colexec/values_differ.eg.go \
  pkg/sql/colexec/vec_comparators.eg.go \
  pkg/sql/colexec/colexecagg/hash_any_not_null_agg.eg.go \
  pkg/sql/colexec/colexecagg/hash_avg_agg.eg.go \
  pkg/sql/colexec/colexecagg/hash_bool_and_or_agg.eg.go \
  pkg/sql/colexec/colexecagg/hash_concat_agg.eg.go \
  pkg/sql/colexec/colexecagg/hash_count_agg.eg.go \
  pkg/sql/colexec/colexecagg/hash_default_agg.eg.go \
  pkg/sql/colexec/colexecagg/hash_min_max_agg.eg.go \
  pkg/sql/colexec/colexecagg/hash_sum_agg.eg.go \
  pkg/sql/colexec/colexecagg/hash_sum_int_agg.eg.go \
  pkg/sql/colexec/colexecagg/ordered_any_not_null_agg.eg.go \
  pkg/sql/colexec/colexecagg/ordered_avg_agg.eg.go \
  pkg/sql/colexec/colexecagg/ordered_bool_and_or_agg.eg.go \
  pkg/sql/colexec/colexecagg/ordered_concat_agg.eg.go \
  pkg/sql/colexec/colexecagg/ordered_count_agg.eg.go \
  pkg/sql/colexec/colexecagg/ordered_default_agg.eg.go \
  pkg/sql/colexec/colexecagg/ordered_min_max_agg.eg.go \
  pkg/sql/colexec/colexecagg/ordered_sum_agg.eg.go \
  pkg/sql/colexec/colexecagg/ordered_sum_int_agg.eg.go \
  pkg/sql/colexec/colexecagg/window_avg_agg.eg.go \
  pkg/sql/colexec/colexecagg/window_bool_and_or_agg.eg.go \
  pkg/sql/colexec/colexecagg/window_concat_agg.eg.go \
  pkg/sql/colexec/colexecagg/window_count_agg.eg.go \
  pkg/sql/colexec/colexecagg/window_min_max_agg.eg.go \
  pkg/sql/colexec/colexecagg/window_sum_agg.eg.go \
  pkg/sql/colexec/colexecagg/window_sum_int_agg.eg.go \
  pkg/sql/colexec/colexecbase/cast.eg.go \
  pkg/sql/colexec/colexecbase/const.eg.go \
  pkg/sql/colexec/colexecbase/distinct.eg.go \
  pkg/sql/colexec/colexeccmp/default_cmp_expr.eg.go \
  pkg/sql/colexec/colexechash/hashtable_distinct.eg.go \
  pkg/sql/colexec/colexechash/hashtable_full_default.eg.go \
  pkg/sql/colexec/colexechash/hashtable_full_deleting.eg.go \
  pkg/sql/colexec/colexechash/hash_utils.eg.go \
  pkg/sql/colexec/colexecjoin/crossjoiner.eg.go \
  pkg/sql/colexec/colexecjoin/hashjoiner.eg.go \
  pkg/sql/colexec/colexecjoin/mergejoinbase.eg.go \
  pkg/sql/colexec/colexecjoin/mergejoiner_exceptall.eg.go \
  pkg/sql/colexec/colexecjoin/mergejoiner_fullouter.eg.go \
  pkg/sql/colexec/colexecjoin/mergejoiner_inner.eg.go \
  pkg/sql/colexec/colexecjoin/mergejoiner_intersectall.eg.go \
  pkg/sql/colexec/colexecjoin/mergejoiner_leftanti.eg.go \
  pkg/sql/colexec/colexecjoin/mergejoiner_leftouter.eg.go \
  pkg/sql/colexec/colexecjoin/mergejoiner_leftsemi.eg.go \
  pkg/sql/colexec/colexecjoin/mergejoiner_rightanti.eg.go \
  pkg/sql/colexec/colexecjoin/mergejoiner_rightouter.eg.go \
  pkg/sql/colexec/colexecjoin/mergejoiner_rightsemi.eg.go \
  pkg/sql/colexec/colexecproj/default_cmp_proj_ops.eg.go \
  pkg/sql/colexec/colexecproj/proj_const_left_ops.eg.go \
  pkg/sql/colexec/colexecproj/proj_const_right_ops.eg.go \
  pkg/sql/colexec/colexecproj/proj_like_ops.eg.go \
  pkg/sql/colexec/colexecproj/proj_non_const_ops.eg.go \
  pkg/sql/colexec/colexecsel/default_cmp_sel_ops.eg.go \
  pkg/sql/colexec/colexecsel/selection_ops.eg.go \
  pkg/sql/colexec/colexecsel/sel_like_ops.eg.go \
  pkg/sql/colexec/colexecspan/span_encoder.eg.go \
  pkg/sql/colexec/colexecwindow/first_value.eg.go \
  pkg/sql/colexec/colexecwindow/lag.eg.go \
  pkg/sql/colexec/colexecwindow/last_value.eg.go \
  pkg/sql/colexec/colexecwindow/lead.eg.go \
  pkg/sql/colexec/colexecwindow/min_max_removable_agg.eg.go \
  pkg/sql/colexec/colexecwindow/ntile.eg.go \
  pkg/sql/colexec/colexecwindow/nth_value.eg.go \
  pkg/sql/colexec/colexecwindow/range_offset_handler.eg.go \
  pkg/sql/colexec/colexecwindow/rank.eg.go \
  pkg/sql/colexec/colexecwindow/relative_rank.eg.go \
  pkg/sql/colexec/colexecwindow/row_number.eg.go \
  pkg/sql/colexec/colexecwindow/window_aggregator.eg.go \
  pkg/sql/colexec/colexecwindow/window_framer.eg.go \
  pkg/sql/colexec/colexecwindow/window_peer_grouper.eg.go

OPTGEN_TARGETS = \
	pkg/sql/opt/memo/expr.og.go \
	pkg/sql/opt/operator.og.go \
	pkg/sql/opt/xform/explorer.og.go \
	pkg/sql/opt/norm/factory.og.go \
	pkg/sql/opt/rule_name.og.go \
	pkg/sql/opt/rule_name_string.go \
	pkg/sql/opt/exec/factory.og.go \
	pkg/sql/opt/exec/explain/explain_factory.og.go \
	pkg/sql/opt/exec/explain/plan_gist_factory.og.go \

test-targets := \
	check test testshort testslow testrace testraceslow testdeadlock testbuild \
	stress stressrace \
	roachprod-stress roachprod-stressrace \
	testlogic testbaselogic testoptlogic

go-targets := $(COCKROACHOSS) $(COCKROACHSHORT) $(COCKROACHSQL) \
	bin/workload bin/roachprod bin/roachtest \
	go-install \
	bench benchshort \
	check test testshort testslow testrace testraceslow testdeadlock testbuild \
	stress stressrace \
	roachprod-stress roachprod-stressrace \
	generate \
	lint lintshort

.DEFAULT_GOAL := all
all: buildoss

.PHONY: c-deps
c-deps: $(C_LIBS_OSS) | $(C_LIBS_DYNAMIC)

build-mode = build -o $@

go-install: build-mode = install

$(COCKROACHOSS): BUILDTARGET = ./pkg/cmd/cockroach-oss
$(COCKROACHOSS): $(C_LIBS_OSS) pkg/ui/assets.oss.installed | $(C_LIBS_DYNAMIC)

$(COCKROACHSHORT): BUILDTARGET = ./pkg/cmd/cockroach-short
$(COCKROACHSHORT): TAGS += short
$(COCKROACHSHORT): $(C_LIBS_SHORT) | $(C_LIBS_DYNAMIC)

$(COCKROACHSQL): BUILDTARGET = ./pkg/cmd/cockroach-sql

# For test targets, add a tag (used to enable extra assertions).
$(test-targets): TAGS += crdb_test

$(go-targets): $(C_LIBS_OSS) | $(C_LIBS_DYNAMIC)

BUILDINFO = .buildinfo/tag .buildinfo/rev
BUILD_TAGGED_RELEASE =

## Override for .buildinfo/tag
BUILDINFO_TAG :=

$(go-targets): bin/.bootstrap $(BUILDINFO) $(CGO_FLAGS_FILES) $(PROTOBUF_TARGETS) $(LIBPROJ) $(GENERATED_TARGETS)
$(go-targets): $(LOG_TARGETS) $(SQLPARSER_TARGETS) $(OPTGEN_TARGETS)
$(go-targets): override LINKFLAGS += \
	-X "github.com/cockroachdb/cockroach/pkg/build.tag=$(if $(BUILDINFO_TAG),$(BUILDINFO_TAG),$(shell cat .buildinfo/tag))" \
	-X "github.com/cockroachdb/cockroach/pkg/build.rev=$(shell cat .buildinfo/rev)" \
	-X "github.com/cockroachdb/cockroach/pkg/build.cgoTargetTriple=$(TARGET_TRIPLE)" \
	$(if $(BUILDCHANNEL),-X "github.com/cockroachdb/cockroach/pkg/build.channel=$(BUILDCHANNEL)") \
	$(if $(BUILD_TAGGED_RELEASE),-X "github.com/cockroachdb/cockroach/pkg/util/log/logcrash.crashReportEnv=$(if $(BUILDINFO_TAG),$(BUILDINFO_TAG),$(shell cat .buildinfo/tag))")

# The build.utcTime format must remain in sync with TimeFormat in
# pkg/build/info.go. It is not installed in tests or in `buildshort` to avoid
# busting the cache on every rebuild.
$(COCKROACHOSS) go-install: override LINKFLAGS += \
	-X "github.com/cockroachdb/cockroach/pkg/build.utcTime=$(shell date -u '+%Y/%m/%d %H:%M:%S')"

settings-doc-gen = $(if $(filter buildshort,$(MAKECMDGOALS)),$(COCKROACHSHORT),$(COCKROACHOSS))

docs/generated/settings/settings.html: $(settings-doc-gen)
	@$(settings-doc-gen) gen settings-list --format=rawhtml > $@

docs/generated/settings/settings-for-tenants.txt:  $(settings-doc-gen)
	@$(settings-doc-gen) gen settings-list --without-system-only > $@

SETTINGS_DOC_PAGES := docs/generated/settings/settings.html docs/generated/settings/settings-for-tenants.txt

# Note: We pass `-v` to `go build` and `go test -i` so that warnings
# from the linker aren't suppressed. The usage of `-v` also shows when
# dependencies are rebuilt which is useful when switching between
# normal and race test builds.
.PHONY: go-install
$(COCKROACHOSS) $(COCKROACHSHORT) $(COCKROACHSQL) go-install:
	 $(xgo) $(build-mode) -v $(GOFLAGS) $(GOMODVENDORFLAGS) -tags '$(TAGS)' -ldflags '$(LINKFLAGS)' $(BUILDTARGET)

# The build targets, in addition to producing a Cockroach binary, silently
# regenerate SQL diagram BNFs and some other doc pages. Generating these docs
# doesn't really belong in the build target, but when they were only part of the
# generate target it was too easy to forget to regenerate them when necessary
# and burn a CI cycle.
#
# We check these docs into version control in the first place in the hope that
# the diff of the generated docs that shows up in Reviewable, 'git diff', etc.
# makes it obvious when a commit has broken the docs. For example, it's very
# easy for changes to the SQL parser to result in unintelligible railroad
# diagrams. When the generated files are not checked in, the breakage goes
# unnoticed until the docs team comes along, potentially months later. Much
# better to make the developer who introduces the breakage fix the breakage.
.PHONY: buildoss buildshort
buildoss: ## Build the CockroachDB binary.
buildshort: ## Build the CockroachDB binary without the admin UI and RocksDB.
buildoss: $(COCKROACHOSS)
buildshort: $(COCKROACHSHORT)
buildoss buildshort: $(if $(is-cross-compile),,$(DOCGEN_TARGETS))
buildshort: $(if $(is-cross-compile),,$(SETTINGS_DOC_PAGES))

.PHONY: install
install: ## Install the CockroachDB binary.
install: $(COCKROACHOSS)
	$(INSTALL) -d -m 755 $(DESTDIR)$(bindir)
	$(INSTALL) -m 755 $(COCKROACHOSS) $(DESTDIR)$(bindir)/cockroach

.PHONY: start
start: $(COCKROACHOSS)
start:
	$(COCKROACHOSS) start $(STARTFLAGS)

# Build, but do not run the tests.
# PKG is expanded and all packages are built and moved to their directory.
.PHONY: testbuild
testbuild:
	$(xgo) list -tags '$(TAGS)' -f \
	'$(xgo) test -v $(GOFLAGS) $(GOMODVENDORFLAGS) -tags '\''$(TAGS)'\'' -ldflags '\''$(LINKFLAGS)'\'' -c {{.ImportPath}} -o {{.Dir}}/{{.Name}}.test' $(PKG) | \
	$(SHELL)

testshort: override TESTFLAGS += -short

testdeadlock: TAGS += deadlock

testrace: ## Run tests with the Go race detector enabled.
testrace stressrace roachprod-stressrace: override GOFLAGS += -race
testrace stressrace roachprod-stressrace: export GORACE := halt_on_error=1
testrace stressrace roachprod-stressrace: TESTTIMEOUT := $(RACETIMEOUT)

# Directory scans in the builder image are excruciatingly slow when running
# Docker for Mac, so we filter out the 20k+ UI dependencies that are
# guaranteed to be irrelevant to save nearly 10s on every Make invocation.
FIND_RELEVANT := find ./pkg -name node_modules -prune -o

bench: ## Run benchmarks.
bench benchshort: TESTS := -
bench benchshort: BENCHES := .
bench benchshort: TESTTIMEOUT := $(BENCHTIMEOUT)

# -benchtime=1ns runs one iteration of each benchmark. The -short flag is set so
# that longer running benchmarks can skip themselves.
benchshort: override TESTFLAGS += -benchtime=1ns -short

.PHONY: check test testshort testrace testdeadlock testlogic testbaselogic testoptlogic bench benchshort
test: ## Run tests.
check test testshort testrace testdeadlock bench benchshort:
	$(xgo) test $(GOTESTFLAGS) $(GOFLAGS) $(GOMODVENDORFLAGS) -tags '$(TAGS)' -ldflags '$(LINKFLAGS)' -run "$(TESTS)" $(if $(BENCHES),-bench "$(BENCHES)") -timeout $(TESTTIMEOUT) $(PKG) $(TESTFLAGS)

.PHONY: stress stressrace
stress: ## Run tests under stress.
stressrace: ## Run tests under stress with the race detector enabled.
stress stressrace:
	$(xgo) test $(GOTESTFLAGS) $(GOFLAGS) $(GOMODVENDORFLAGS) -exec 'stress $(STRESSFLAGS)' -tags '$(TAGS)' -ldflags '$(LINKFLAGS)' -run "$(TESTS)" -timeout 0 $(PKG) $(filter-out -v,$(TESTFLAGS)) -v -args -test.timeout $(TESTTIMEOUT)

.PHONY: roachprod-stress roachprod-stressrace
roachprod-stress roachprod-stressrace: bin/roachprod-stress
	# The bootstrap target creates, among other things, ./bin/stress.
	@if [ -z "$(CLUSTER)" ]; then \
		echo "ERROR: missing or empty CLUSTER; create one via:"; \
		echo "roachprod create \$$USER-stress -n 20 --gce-machine-type=n1-standard-8 --local-ssd=false"; \
		exit 1; \
	fi
	build/builder.sh make bin/.bootstrap
	build/builder.sh mkrelease amd64-linux-gnu test GOFLAGS="$(GOFLAGS)" TESTFLAGS="-v -c -o $(notdir $(patsubst %/,%,$(PKG))).test" PKG=$(PKG) TAGS="$(TAGS)"
	bin/roachprod-stress $(CLUSTER) $(patsubst github.com/cockroachdb/cockroach/%,./%,$(PKG)) $(STRESSFLAGS) -- \
	  -test.run "$(TESTS)" $(filter-out -v,$(TESTFLAGS)) -test.v -test.timeout $(TESTTIMEOUT); \

testlogic: testbaselogic testoptlogic

testbaselogic: ## Run SQL Logic Tests.
testbaselogic: bin/logictest

testoptlogic: ## Run SQL Logic Tests from opt package.
testoptlogic: bin/logictestopt

logic-test-selector := $(if $(TESTCONFIG),^$(TESTCONFIG)$$)/$(if $(FILES),^$(subst $(space),$$|^,$(FILES))$$)/$(SUBTESTS)
testbaselogic: TESTS := TestLogic/$(logic-test-selector)
testoptlogic: TESTS := TestExecBuild/$(logic-test-selector)

# Note: we specify -config here in addition to the filter on TESTS
# above. This is because if we only restrict in TESTS, this will
# merely cause Go to skip the sub-tests that match the pattern. It
# does not prevent loading and initializing every default config in
# turn (including setting up the test clusters, etc.). By specifying
# -config, the extra initialization overhead is averted.
testbaselogic testoptlogic: TESTFLAGS := -test.v $(if $(FILES),-show-sql) $(if $(TESTCONFIG),-config $(TESTCONFIG))
testbaselogic testoptlogic:
	cd $($(<F)-package) && $(<F) -test.run "$(TESTS)" -test.timeout $(TESTTIMEOUT) $(TESTFLAGS)

testraceslow: override GOFLAGS += -race
testraceslow: TESTTIMEOUT := $(RACETIMEOUT)

.PHONY: testslow testraceslow
testslow testraceslow: override TESTFLAGS += -v
testslow testraceslow:
	$(xgo) test $(GOTESTFLAGS) $(GOFLAGS) $(GOMODVENDORFLAGS) -tags '$(TAGS)' -ldflags '$(LINKFLAGS)' -run "$(TESTS)" $(if $(BENCHES),-bench "$(BENCHES)") -timeout $(TESTTIMEOUT) $(PKG) $(TESTFLAGS) | grep -F ': Test' | sed -E 's/(--- PASS: |\(|\))//g' | awk '{ print $$2, $$1 }' | sort -rn | head -n 10

.PHONY: acceptance
acceptance: TESTTIMEOUT := $(ACCEPTANCETIMEOUT)
acceptance: export TESTTIMEOUT := $(TESTTIMEOUT)
acceptance: ## Run acceptance tests.
	+@pkg/acceptance/run.sh

.PHONY: compose
compose: export TESTTIMEOUT := $(TESTTIMEOUT)
compose: ## Run compose tests.
	+@pkg/compose/run.sh

.PHONY: dupl
dupl: bin/.bootstrap
	$(FIND_RELEVANT) \
	       -name '*.go'             \
	       -not -name '*.pb.go'    	\
	       -not -name '*.pb.gw.go' 	\
	       -not -name '*_string.go'	\
	       -not -name 'sql.go'     	\
	       -not -name 'irgen.go'   	\
	       -not -name '*.ir.go'    	\
	| dupl -files $(DUPLFLAGS)

.PHONY: generate
generate: ## Regenerate generated code.
generate: protobuf $(DOCGEN_TARGETS) $(OPTGEN_TARGETS) $(LOG_TARGETS) $(SQLPARSER_TARGETS) $(SETTINGS_DOC_PAGES) $(SWAGGER_TARGETS) bin/langgen bin/terraformgen
	$(GO) generate $(GOFLAGS) $(GOMODVENDORFLAGS) -tags '$(TAGS)' -ldflags '$(LINKFLAGS)' $(PKG)
	$(MAKE) execgen

lint lintshort: TESTTIMEOUT := $(LINTTIMEOUT)

.PHONY: lint
lint: override TAGS += lint
lint: ## Run all style checkers and linters.
lint: bin/returncheck bin/roachvet bin/optfmt
	@if [ -t 1 ]; then echo '$(yellow)NOTE: `make lint` is very slow! Perhaps `make lintshort`?$(term-reset)'; fi
	@# Run 'go build -i' to ensure we have compiled object files available for all
	@# packages. In Go 1.10, only 'go vet' recompiles on demand. For details:
	@# https://groups.google.com/forum/#!msg/golang-dev/qfa3mHN4ZPA/X2UzjNV1BAAJ.
	$(xgo) build -i -v $(GOFLAGS) $(GOMODVENDORFLAGS) -tags '$(TAGS)' -ldflags '$(LINKFLAGS)' $(PKG)
	$(xgo) test $(GOTESTFLAGS) ./pkg/testutils/lint -v $(GOFLAGS) $(GOMODVENDORFLAGS) -tags '$(TAGS)' -ldflags '$(LINKFLAGS)' -timeout $(TESTTIMEOUT) -run 'Lint/$(TESTS)'

.PHONY: lintshort
lintshort: override TAGS += lint
lintshort: ## Run a fast subset of the style checkers and linters.
lintshort: bin/roachvet bin/optfmt
	$(xgo) test $(GOTESTFLAGS) ./pkg/testutils/lint -v $(GOFLAGS) $(GOMODVENDORFLAGS) -tags '$(TAGS)' -ldflags '$(LINKFLAGS)' -short -timeout $(TESTTIMEOUT) -run 'TestLint/$(TESTS)'

.PHONY: protobuf
protobuf: $(PROTOBUF_TARGETS)
protobuf: ## Regenerate generated code for protobuf definitions.

# pre-push locally runs most of the checks CI will run. Notably, it doesn't run
# the acceptance tests.
.PHONY: pre-push
pre-push: ## Run generate, lint, and test.
pre-push: generate lint test ui-lint ui-test
	! git status --porcelain | read || (git status; git --no-pager diff -a 1>&2; exit 1)

cockroach.tgz: ## Build a binary tarball.
cockroach.tgz: $(COCKROACHOSS) $(LIBGEOS)
	rm -rf artifacts/cockroach
	mkdir -p artifacts/cockroach/lib
	ln $(COCKROACHOSS) artifacts/cockroach/cockroach
	ln $(DYN_LIB_DIR)/libgeos.$(DYN_EXT) $(DYN_LIB_DIR)/libgeos_c.$(DYN_EXT) artifacts/cockroach/lib/
	tar -cvf - -C artifacts cockroach | gzip -9 > $@
	sha256sum $@ | awk '{ print $$1 }' > $@.sha256

# archive builds a source tarball out of this repository. Files in the special
# directory build/archive/contents are inserted directly into $(ARCHIVE_BASE).
# All other files in the repository are inserted into the archive with prefix
# $(ARCHIVE_BASE)/src/github.com/cockroachdb/cockroach to allow the extracted
# archive to serve directly as a GOPATH root.
.PHONY: archive
archive: ## Build a source tarball from this repository.
archive: $(ARCHIVE)

$(ARCHIVE): $(ARCHIVE).tmp
	gzip -c $< > $@

# ARCHIVE_EXTRAS are hard-to-generate files and their prerequisites that are
# pre-generated and distributed in source archives to minimize the number of
# dependencies required for end-users to build from source.
ARCHIVE_EXTRAS = \
	$(BUILDINFO) \
	$(SQLPARSER_TARGETS) \
	$(OPTGEN_TARGETS) \
	pkg/ui/assets.oss.installed

# TODO(benesch): Make this recipe use `git ls-files --recurse-submodules`
# instead of scripts/ls-files.sh once Git v2.11 is widely deployed.
.INTERMEDIATE: $(ARCHIVE).tmp
$(ARCHIVE).tmp: ARCHIVE_BASE = cockroach-$(if $(BUILDINFO_TAG),$(BUILDINFO_TAG),$(shell cat .buildinfo/tag))
$(ARCHIVE).tmp: $(ARCHIVE_EXTRAS)
	echo "$(if $(BUILDINFO_TAG),$(BUILDINFO_TAG),$(shell cat .buildinfo/tag))" > .buildinfo/tag
	scripts/ls-files.sh | $(TAR) -cf $@ -T - $(TAR_XFORM_FLAG),^,$(ARCHIVE_BASE)/src/github.com/cockroachdb/cockroach/, $^
	(cd build/archive/contents && $(TAR) -rf ../../../$@ $(TAR_XFORM_FLAG),^,$(ARCHIVE_BASE)/, *)

.buildinfo:
	@mkdir -p $@

# Do not use plumbing commands, like git diff-index, in this target. Our build
# process modifies files quickly enough that plumbing commands report false
# positives on filesystems with only one second of resolution as a performance
# optimization. Porcelain commands, like git diff, exist to detect and remove
# these false positives.
#
# For details, see the "Possible timestamp problems with diff-files?" thread on
# the Git mailing list (http://marc.info/?l=git&m=131687596307197).
.buildinfo/tag: | .buildinfo
	@{ git describe --tags --dirty --match=v[0-9]* 2> /dev/null || git rev-parse --short HEAD; } | tr -d \\n > $@

.buildinfo/rev: | .buildinfo
	@git rev-parse HEAD > $@

ifneq ($(GIT_DIR),)
# If we're in a Git checkout, we update the buildinfo information on every build
# to keep it up-to-date.
.buildinfo/tag: .ALWAYS_REBUILD
.buildinfo/rev: .ALWAYS_REBUILD
endif

GOGO_PROTOBUF_PATH := ./vendor/github.com/gogo/protobuf

GOGOPROTO_PROTO := $(GOGO_PROTOBUF_PATH)/gogoproto/gogo.proto

PROMETHEUS_PATH := ./vendor/github.com/prometheus/client_model

ERRORS_PATH := ./vendor/github.com/cockroachdb/errors
ERRORS_PROTO := $(ERRORS_PATH)/errorspb/errors.proto

COREOS_PATH := ./vendor/go.etcd.io

GRPC_GATEWAY_GOOGLEAPIS_PACKAGE := github.com/grpc-ecosystem/grpc-gateway/third_party/googleapis
GRPC_GATEWAY_GOOGLEAPIS_PATH := ./vendor/$(GRPC_GATEWAY_GOOGLEAPIS_PACKAGE)

# Map protobuf includes to the Go package containing the generated Go code.
PROTO_MAPPINGS :=
PROTO_MAPPINGS := $(PROTO_MAPPINGS)Mgoogle/api/annotations.proto=google.golang.org/genproto/googleapis/api/annotations,
PROTO_MAPPINGS := $(PROTO_MAPPINGS)Mgoogle/protobuf/timestamp.proto=github.com/gogo/protobuf/types,
PROTO_MAPPINGS := $(PROTO_MAPPINGS)Mgoogle/protobuf/any.proto=github.com/gogo/protobuf/types,
PROTO_MAPPINGS := $(PROTO_MAPPINGS)Mgoogle/protobuf/duration.proto=github.com/gogo/protobuf/types,

GW_SERVER_PROTOS := ./pkg/server/serverpb/admin.proto ./pkg/server/serverpb/status.proto ./pkg/server/serverpb/authentication.proto
GW_TS_PROTOS := ./pkg/ts/tspb/timeseries.proto

GW_PROTOS  := $(GW_SERVER_PROTOS) $(GW_TS_PROTOS)
GW_SOURCES := $(GW_PROTOS:%.proto=%.pb.gw.go)

GO_PROTOS := $(sort $(shell $(FIND_RELEVANT) -type f -name '*.proto' -print))
GO_SOURCES := $(GO_PROTOS:%.proto=%.pb.go)

PBJS := $(NODE_RUN) pkg/ui/node_modules/.bin/pbjs
PBTS := $(NODE_RUN) pkg/ui/node_modules/.bin/pbts

# Unlike the protobuf compiler for Go and C++, the protobuf compiler for
# JavaScript only needs the entrypoint protobufs to be listed. It automatically
# compiles any protobufs the entrypoints depend upon.
UI_JS_OSS := pkg/ui/workspaces/db-console/src/js/protos.js
UI_TS_OSS := pkg/ui/workspaces/db-console/src/js/protos.d.ts
UI_PROTOS_OSS := $(UI_JS_OSS) $(UI_TS_OSS)

$(GOGOPROTO_PROTO): vendor/modules.txt
$(ERRORS_PROTO): vendor/modules.txt

bin/.go_protobuf_sources: $(GO_PROTOS) $(GOGOPROTO_PROTO) $(ERRORS_PROTO) bin/.bootstrap bin/protoc-gen-gogoroach c-deps/proto-rebuild
	$(FIND_RELEVANT) -type f -name '*.pb.go' -exec rm {} +
	set -e; for dir in $(sort $(dir $(GO_PROTOS))); do \
	  buf protoc -Ipkg -I$(GOGO_PROTOBUF_PATH) -I$(COREOS_PATH) -I$(PROMETHEUS_PATH) -I$(GRPC_GATEWAY_GOOGLEAPIS_PATH) -I$(ERRORS_PATH) --gogoroach_out=$(PROTO_MAPPINGS)plugins=grpc,import_prefix=github.com/cockroachdb/cockroach/pkg/:./pkg $$dir/*.proto; \
	done
	gofmt -s -w $(GO_SOURCES)
	touch $@

bin/.gw_protobuf_sources: $(GW_SERVER_PROTOS) $(GW_TS_PROTOS) $(GO_PROTOS) $(GOGOPROTO_PROTO) $(ERRORS_PROTO) bin/.bootstrap c-deps/proto-rebuild
	$(FIND_RELEVANT) -type f -name '*.pb.gw.go' -exec rm {} +
		buf protoc -Ipkg -I$(GOGO_PROTOBUF_PATH) -I$(ERRORS_PATH) -I$(COREOS_PATH) -I$(PROMETHEUS_PATH) -I$(GRPC_GATEWAY_GOOGLEAPIS_PATH) --grpc-gateway_out=logtostderr=true,request_context=true:./pkg $(GW_SERVER_PROTOS)
		buf protoc -Ipkg -I$(GOGO_PROTOBUF_PATH) -I$(ERRORS_PATH) -I$(COREOS_PATH) -I$(PROMETHEUS_PATH) -I$(GRPC_GATEWAY_GOOGLEAPIS_PATH) --grpc-gateway_out=logtostderr=true,request_context=true:./pkg $(GW_TS_PROTOS)
	gofmt -s -w $(GW_SOURCES)
	@# TODO(jordan,benesch) This can be removed along with the above TODO.
	goimports -w $(GW_SOURCES)
	touch $@

.SECONDARY: $(UI_JS_OSS)
$(UI_JS_OSS): $(GW_PROTOS) $(GO_PROTOS) pkg/ui/yarn.installed vendor/modules.txt
	# Add comment recognized by reviewable.
	echo '// GENERATED FILE DO NOT EDIT' > $@
	$(PBJS) -t static-module -w es6 --strict-long --no-create --no-convert --no-delimited --no-verify --keep-case --path pkg --path ./vendor/github.com --path $(GOGO_PROTOBUF_PATH) --path $(ERRORS_PATH) --path $(COREOS_PATH) --path $(PROMETHEUS_PATH) --path $(GRPC_GATEWAY_GOOGLEAPIS_PATH) $(filter %.proto,$(GW_PROTOS)) >> $@

# End of PBJS-generated files.

.SECONDARY: $(UI_TS_OSS)
$(UI_TS_OSS): $(UI_JS_OSS) pkg/ui/yarn.installed
$(UI_TS_OSS):
	# Add comment recognized by reviewable.
	echo '// GENERATED FILE DO NOT EDIT' > $@
	$(PBTS) $< >> $@

STYLINT            := ./node_modules/.bin/stylint
TSC                := ./node_modules/.bin/tsc
KARMA              := ./node_modules/.bin/karma
WEBPACK            := ./node_modules/.bin/webpack
WEBPACK_DEV_SERVER := ./node_modules/.bin/webpack-dev-server
WEBPACK_DASHBOARD  := ./opt/node_modules/.bin/webpack-dashboard

.PHONY: ui-generate
ui-generate: pkg/ui/assets.oss.installed

.PHONY: ui-fonts
ui-fonts:
	pkg/ui/workspaces/db-console/scripts/font-gen

.PHONY: ui-topo
ui-topo: pkg/ui/yarn.installed
	pkg/ui/workspaces/db-console/scripts/topo.js

.PHONY: ui-lint
ui-lint: pkg/ui/yarn.installed $(UI_PROTOS_OSS)
	$(NODE_RUN) -C pkg/ui/workspaces/db-console $(STYLINT) -c .stylintrc styl
	$(NODE_RUN) -C pkg/ui/workspaces/db-console $(TSC)
	$(NODE_RUN) -C pkg/ui/workspaces/db-console yarn lint
	@if $(NODE_RUN) -C pkg/ui/workspaces/db-console yarn list | grep phantomjs; then echo ^ forbidden UI dependency >&2; exit 1; fi
	$(NODE_RUN) -C pkg/ui/workspaces/cluster-ui yarn --cwd pkg/ui/workspaces/cluster-ui lint

# DLLs are Webpack bundles, not Windows shared libraries. See "DLLs for speedy
# builds" in the UI README for details.
UI_OSS_DLLS := pkg/ui/workspaces/db-console/dist/protos.oss.dll.js pkg/ui/workspaces/db-console/dist/vendor.oss.dll.js
UI_OSS_MANIFESTS := pkg/ui/workspaces/db-console/protos.oss.manifest.json pkg/ui/workspaces/db-console/vendor.oss.manifest.json

# (Ab)use pattern rules to teach Make that this one Webpack command produces two
# files. Normally, Make would run the recipe twice if dist/FOO.js and
# FOO-manifest.js were both out-of-date. [0]
#
# TODO(irfansharif): Ideally we'd scope the dependency on $(UI_PROTOS*) to the
# appropriate protos DLLs, but Make v3.81 has a bug that causes the dependency
# to be ignored [1]. We're stuck with this workaround until Apple decides to
# update the version of Make they ship with macOS or we require a newer version
# of Make. Such a requirement would need to be strictly enforced, as the way
# this fails is extremely subtle and doesn't present until the web UI is loaded
# in the browser.
#
# [0]: https://stackoverflow.com/a/3077254/1122351
# [1]: http://savannah.gnu.org/bugs/?19108
.SECONDARY: $(UI_OSS_DLLS) $(UI_OSS_MANIFESTS)

pkg/ui/workspaces/db-console/dist/%.oss.dll.js pkg/ui/workspaces/db-console/%.oss.manifest.json: export NODE_OPTIONS=--max-old-space-size=5000
pkg/ui/workspaces/db-console/dist/%.oss.dll.js pkg/ui/workspaces/db-console/%.oss.manifest.json: pkg/ui/workspaces/db-console/webpack.%.js pkg/ui/yarn.installed $(CLUSTER_UI_JS) $(UI_PROTOS_OSS)
	$(NODE_RUN) -C pkg/ui/workspaces/db-console $(WEBPACK) -p --config webpack.$*.js --env.dist=oss

.PHONY: ui-test
ui-test: $(UI_OSS_DLLS) $(UI_OSS_MANIFESTS)
	$(NODE_RUN) -C pkg/ui/workspaces/db-console $(KARMA) start
	$(NODE_RUN) -C pkg/ui/workspaces/cluster-ui yarn ci

.PHONY: ui-test-watch
ui-test-watch: $(UI_OSS_DLLS) $(UI_OSS_MANIFESTS)
	$(NODE_RUN) -C pkg/ui/workspaces/db-console $(KARMA) start --no-single-run --auto-watch & \
	$(NODE_RUN) -C pkg/ui/workspaces/cluster-ui yarn test

.PHONY: ui-test-debug
ui-test-debug: $(UI_DLLS) $(UI_MANIFESTS)
	$(NODE_RUN) -C pkg/ui/workspaces/db-console $(KARMA) start --browsers Chrome --no-single-run --debug --auto-watch

.SECONDARY: pkg/ui/assets.oss.installed
pkg/ui/assets.oss.installed: $(UI_OSS_DLLS) $(UI_OSS_MANIFESTS) $(UI_JS_OSS)
pkg/ui/assets.oss.installed: pkg/ui/workspaces/db-console/webpack.app.js $(shell find pkg/ui/workspaces/db-console/src pkg/ui/workspaces/db-console/styl -type f) | bin/.bootstrap
	find pkg/ui/distoss/assets -mindepth 1 -not -name .gitkeep -delete
	export NODE_OPTIONS=--max-old-space-size=5000
	$(NODE_RUN) -C pkg/ui/workspaces/db-console $(WEBPACK) --config webpack.app.js --env.dist=oss
	touch $@

pkg/ui/yarn.opt.installed:
	$(NODE_RUN) -C pkg/ui yarn install --check-files --offline
	touch $@

.PHONY: ui-watch-secure
ui-watch-secure: override WEBPACK_DEV_SERVER_FLAGS += --https
ui-watch-secure: export TARGET ?= https://localhost:8080/

.PHONY: ui-watch
ui-watch: export TARGET ?= http://localhost:8080
ui-watch ui-watch-secure: PORT := 3000
ui-watch ui-watch-secure: $(UI_OSS_DLLS) pkg/ui/yarn.opt.installed
  # TODO (koorosh): running two webpack dev servers doesn't provide best performance and polling changes.
  # it has to be considered to use something like `parallel-webpack` lib.
  #
  # `node-run.sh` wrapper is removed because this command is supposed to be run in dev environment (not in docker of CI)
  # so it is safe to run yarn commands directly to preserve formatting and colors for outputs
	yarn --cwd pkg/ui/workspaces/cluster-ui build:watch & \
	yarn --cwd pkg/ui/workspaces/db-console webpack-dev-server --config webpack.app.js --env.dist=oss --env.WEBPACK_SERVE --port $(PORT) --mode "development" $(WEBPACK_DEV_SERVER_FLAGS)

.PHONY: ui-clean
ui-clean: ## Remove build artifacts.
	find pkg/ui/distoss/assets -mindepth 1 -not -name .gitkeep -delete
	rm -rf pkg/ui/assets.oss.installed
	rm -rf pkg/ui/dist_vendor/*
	rm -f $(UI_PROTOS_OSS)
	rm -f pkg/ui/workspaces/db-console/*manifest.json
	rm -rf pkg/ui/workspaces/cluster-ui/dist

.PHONY: ui-maintainer-clean
ui-maintainer-clean: ## Like clean, but also remove installed dependencies
ui-maintainer-clean: ui-clean
	rm -rf pkg/ui/node_modules pkg/ui/workspaces/db-console/node_modules pkg/ui/yarn.installed pkg/ui/workspaces/cluster-ui/node_modules pkg/ui/workspaces/db-console/src/js/node_modules

pkg/roachprod/vm/aws/embedded.go: bin/.bootstrap pkg/roachprod/vm/aws/config.json pkg/roachprod/vm/aws/old.json bin/terraformgen
	(cd pkg/roachprod/vm/aws && $(GO) generate)

pkg/security/securitytest/embedded.go: bin/.bootstrap $(shell find pkg/security/securitytest/test_certs -type f -not -name README.md -not -name regenerate.sh)
	(cd pkg/security/securitytest && $(GO) generate)

.SECONDARY: pkg/sql/parser/gen/sql.go.tmp
pkg/sql/parser/gen/sql.go.tmp: pkg/sql/parser/gen/sql-gen.y bin/.bootstrap
	set -euo pipefail; \
	  ret=$$(cd pkg/sql/parser/gen && goyacc -p sql -o sql.go.tmp sql-gen.y); \
	  if expr "$$ret" : ".*conflicts" >/dev/null; then \
	    echo "$$ret"; exit 1; \
	  fi

# The lex package needs to know about all tokens, because the encode
# functions and lexing predicates need to know about keywords, and
# keywords map to the token constants. Therefore, generate the
# constant tokens in the lex package primarily.
pkg/sql/lexbase/tokens.go: pkg/sql/parser/gen/sql.go.tmp
	(echo "// Code generated by make. DO NOT EDIT."; \
	 echo "// GENERATED FILE DO NOT EDIT"; \
	 echo; \
	 echo "package lexbase"; \
	 echo; \
	 grep '^const [A-Z][_A-Z0-9]* ' $^) > $@.tmp || rm $@.tmp
	mv -f $@.tmp $@

# The lex package is now the primary source for the token constant
# definitions. Modify the code generated by goyacc here to refer to
# the definitions in the lex package.
pkg/sql/parser/sql.go: pkg/sql/parser/gen/sql.go.tmp | bin/.bootstrap
	(echo "// Code generated by goyacc. DO NOT EDIT."; \
	 echo "// GENERATED FILE DO NOT EDIT"; \
	 cat $^ | \
	 sed -E 's/^const ([A-Z][_A-Z0-9]*) =.*$$/const \1 = lexbase.\1/g') > $@.tmp || rm $@.tmp
	mv -f $@.tmp $@
	goimports -w $@

# This modifies the grammar to:
# - improve the types used by the generated parser for non-terminals
# - expand the help rules.
#
# For types:
# Determine the types that will be migrated to union types by looking
# at the accessors of sqlSymUnion. The first step in this pipeline
# prints every return type of a sqlSymUnion accessor on a separate line.
# The next step regular expression escapes these types. The third
# (prepending) and the fourth (appending) steps build regular expressions
# for each of the types and store them in the file. (We make multiple
# regular expressions because we ran into a limit of characters for a
# single regex executed by sed.)
# Then translate the original syntax file, with the types determined
# above being replaced with the union type in their type declarations.
.SECONDARY: pkg/sql/parser/gen/sql-gen.y
pkg/sql/parser/gen/sql-gen.y: pkg/sql/parser/sql.y pkg/sql/parser/replace_help_rules.awk
	mkdir -p pkg/sql/parser/gen
	set -euo pipefail; \
	awk '/func.*sqlSymUnion/ {print $$(NF - 1)}' pkg/sql/parser/sql.y | \
	sed -e 's/[]\/$$*.^|[]/\\&/g' | \
	sed -e "s/^/s_(type|token) <(/" | \
	awk '{print $$0")>_\\1 <union> /* <\\2> */_"}' > pkg/sql/parser/gen/types_regex.tmp; \
	sed -E -f pkg/sql/parser/gen/types_regex.tmp < pkg/sql/parser/sql.y | \
	awk -f pkg/sql/parser/replace_help_rules.awk | \
	sed -Ee 's,//.*$$,,g;s,/[*]([^*]|[*][^/])*[*]/, ,g;s/ +$$//g' > $@.tmp || rm $@.tmp
	mv -f $@.tmp $@
	rm pkg/sql/parser/gen/types_regex.tmp

pkg/sql/lexbase/reserved_keywords.go: pkg/sql/parser/sql.y pkg/sql/parser/reserved_keywords.awk | bin/.bootstrap
	awk -f pkg/sql/parser/reserved_keywords.awk < $< > $@.tmp || rm $@.tmp
	mv -f $@.tmp $@
	gofmt -s -w $@

pkg/sql/lexbase/keywords.go: pkg/sql/parser/sql.y pkg/sql/lexbase/allkeywords/main.go | bin/.bootstrap
	$(GO) run $(GOMODVENDORFLAGS) -tags all-keywords pkg/sql/lexbase/allkeywords/main.go < $< > $@.tmp || rm $@.tmp
	mv -f $@.tmp $@
	gofmt -s -w $@

# This target will print unreserved_keywords which are not actually
# used in the grammar.
.PHONY: sqlparser-unused-unreserved-keywords
sqlparser-unused-unreserved-keywords: pkg/sql/parser/sql.y pkg/sql/parser/unreserved_keywords.awk
	@for kw in $$(awk -f pkg/sql/parser/unreserved_keywords.awk < $<); do \
	  if [ $$(grep -c $${kw} $<) -le 2 ]; then \
	    echo $${kw}; \
	  fi \
	done

pkg/sql/parser/helpmap_test.go: pkg/sql/parser/gen/sql-gen.y pkg/sql/parser/help_gen_test.sh | bin/.bootstrap
	@pkg/sql/parser/help_gen_test.sh < $< >$@.tmp || rm $@.tmp
	mv -f $@.tmp $@
	gofmt -s -w $@

pkg/sql/parser/help_messages.go: pkg/sql/parser/sql.y pkg/sql/parser/help.awk | bin/.bootstrap
	awk -f pkg/sql/parser/help.awk < $< > $@.tmp || rm $@.tmp
	mv -f $@.tmp $@
	gofmt -s -w $@

bin/.docgen_bnfs: bin/docgen
	rm -f docs/generated/sql/bnf/*.bnf
	docgen grammar bnf docs/generated/sql/bnf --quiet
	touch $@

bin/.docgen_functions: bin/docgen
	docgen functions docs/generated/sql --quiet
	touch $@

bin/.docgen_logformats: bin/docgen
	docgen logformats docs/generated/logformats.md
	touch $@

bin/.docgen_http: bin/docgen bin/.bootstrap
	docgen http \
	--gendoc ./bin/protoc-gen-doc \
	--out docs/generated/http \
	--protoc-flags "-Ipkg -I$(GOGO_PROTOBUF_PATH) -I$(COREOS_PATH) -I$(GRPC_GATEWAY_GOOGLEAPIS_PATH) -I$(ERRORS_PATH) -I$(PROMETHEUS_PATH) ./pkg/server/serverpb/status.proto ./pkg/server/serverpb/admin.proto ./pkg/server/status/statuspb/status.proto"
	touch $@

docs/generated/redact_safe.md:
	./build/bazelutil/generate_redact_safe.sh >$@.tmp || { rm -f $@.tmp; exit 1; }
	@mv -f $@.tmp $@

#### WARNING ####
# You must keep this list in sync with the list in `pkg/util/log/eventpb/PROTOS.bzl`.
# Order matters!!
EVENTLOG_PROTOS = \
	pkg/util/log/eventpb/events.proto \
	pkg/util/log/eventpb/debug_events.proto \
	pkg/util/log/eventpb/ddl_events.proto \
	pkg/util/log/eventpb/misc_sql_events.proto \
	pkg/util/log/eventpb/privilege_events.proto \
	pkg/util/log/eventpb/role_events.proto \
	pkg/util/log/eventpb/zone_events.proto \
	pkg/util/log/eventpb/session_events.proto \
	pkg/util/log/eventpb/sql_audit_events.proto \
	pkg/util/log/eventpb/cluster_events.proto \
	pkg/util/log/eventpb/job_events.proto \
	pkg/util/log/eventpb/health_events.proto \
	pkg/util/log/eventpb/telemetry.proto

LOGSINKDOC_DEP = pkg/util/log/logconfig/config.go

docs/generated/logsinks.md: pkg/util/log/logconfig/gen.go $(LOGSINKDOC_DEP) | bin/.bootstrap
	$(GO) run $(GOMODVENDORFLAGS) $< <$(LOGSINKDOC_DEP) >$@.tmp || { rm -f $@.tmp; exit 1; }
	mv -f $@.tmp $@

docs/generated/eventlog.md: pkg/util/log/eventpb/gen.go $(EVENTLOG_PROTOS) | bin/.go_protobuf_sources
	$(GO) run $(GOMODVENDORFLAGS) $< eventlog.md $(EVENTLOG_PROTOS) >$@.tmp || { rm -f $@.tmp; exit 1; }
	mv -f $@.tmp $@

pkg/util/log/eventpb/eventlog_channels_generated.go: pkg/util/log/eventpb/gen.go $(EVENTLOG_PROTOS) | bin/.go_protobuf_sources
	$(GO) run $(GOMODVENDORFLAGS) $< eventlog_channels_go $(EVENTLOG_PROTOS) >$@.tmp || { rm -f $@.tmp; exit 1; }
	mv -f $@.tmp $@

pkg/util/log/eventpb/json_encode_generated.go: pkg/util/log/eventpb/gen.go $(EVENTLOG_PROTOS) | bin/.go_protobuf_sources
	$(GO) run $(GOMODVENDORFLAGS) $< json_encode_go $(EVENTLOG_PROTOS) >$@.tmp || { rm -f $@.tmp; exit 1; }
	mv -f $@.tmp $@

docs/generated/logging.md: pkg/util/log/gen/main.go pkg/util/log/logpb/log.proto | bin/.bootstrap
	$(GO) run $(GOMODVENDORFLAGS) $^ logging.md $@.tmp || { rm -f $@.tmp; exit 1; }
	mv -f $@.tmp $@

docs/generated/swagger/spec.json: pkg/server/api*.go bin/.bootstrap

pkg/util/log/severity/severity_generated.go: pkg/util/log/gen/main.go pkg/util/log/logpb/log.proto | bin/.bootstrap
	$(GO) run $(GOMODVENDORFLAGS) $^ severity.go $@.tmp || { rm -f $@.tmp; exit 1; }
	mv -f $@.tmp $@

pkg/util/log/channel/channel_generated.go: pkg/util/log/gen/main.go pkg/util/log/logpb/log.proto | bin/.bootstrap
	$(GO) run $(GOMODVENDORFLAGS) $^ channel.go $@.tmp || { rm -f $@.tmp; exit 1; }
	mv -f $@.tmp $@

pkg/util/log/log_channels_generated.go: pkg/util/log/gen/main.go pkg/util/log/logpb/log.proto | bin/.bootstrap
	$(GO) run $(GOMODVENDORFLAGS) $^ log_channels.go $@.tmp || { rm -f $@.tmp; exit 1; }
	mv -f $@.tmp $@

.PHONY: execgen
execgen: ## Regenerate generated code for the vectorized execution engine.
execgen: $(EXECGEN_TARGETS) bin/execgen
	for i in $(EXECGEN_TARGETS); do echo EXECGEN $$i && COCKROACH_INTERNAL_DISABLE_METAMORPHIC_TESTING=true ./bin/execgen -fmt=false $$i > $$i; done
	goimports -w $(EXECGEN_TARGETS)

# Add a catch-all rule for any non-existent execgen generated
# files. This prevents build errors when switching between branches
# that introduce or remove execgen generated files as these files are
# persisted in bin/%.d dependency lists (e.g. in bin/logictest.d).
%.eg.go: ;

optgen-defs := pkg/sql/opt/ops/*.opt
optgen-norm-rules := pkg/sql/opt/norm/rules/*.opt
optgen-xform-rules := pkg/sql/opt/xform/rules/*.opt
optgen-exec-defs := pkg/sql/opt/exec/factory.opt

pkg/sql/opt/memo/expr.og.go: $(optgen-defs) bin/optgen
	optgen -out $@ exprs $(optgen-defs)

pkg/sql/opt/operator.og.go: $(optgen-defs) bin/optgen
	optgen -out $@ ops $(optgen-defs)

pkg/sql/opt/rule_name.og.go: $(optgen-defs) $(optgen-norm-rules) $(optgen-xform-rules) bin/optgen
	optgen -out $@ rulenames $(optgen-defs) $(optgen-norm-rules) $(optgen-xform-rules)

pkg/sql/opt/rule_name_string.go: pkg/sql/opt/rule_name.go pkg/sql/opt/rule_name.og.go bin/.bootstrap
	stringer -output=$@ -type=RuleName $(filter %.go,$^)

pkg/sql/opt/xform/explorer.og.go: $(optgen-defs) $(optgen-xform-rules) bin/optgen
	optgen -out $@ explorer $(optgen-defs) $(optgen-xform-rules)

pkg/sql/opt/norm/factory.og.go: $(optgen-defs) $(optgen-norm-rules) bin/optgen
	optgen -out $@ factory $(optgen-defs) $(optgen-norm-rules)

pkg/sql/opt/exec/factory.og.go: $(optgen-defs) $(optgen-exec-defs) bin/optgen
	optgen -out $@ execfactory $(optgen-exec-defs)

pkg/sql/opt/exec/explain/explain_factory.og.go: $(optgen-defs) $(optgen-exec-defs) bin/optgen
	optgen -out $@ execexplain $(optgen-exec-defs)

pkg/sql/opt/exec/explain/plan_gist_factory.og.go: $(optgen-defs) $(optgen-exec-defs) bin/optgen
	optgen -out $@ execplangist $(optgen-exec-defs)

.PHONY: clean-c-deps
clean-c-deps:
	rm -rf $(JEMALLOC_DIR)
	rm -rf $(GEOS_DIR)
	rm -rf $(PROJ_DIR)

.PHONY: unsafe-clean-c-deps
unsafe-clean-c-deps:
	git -C $(JEMALLOC_SRC_DIR) clean -dxf
	git -C $(GEOS_SRC_DIR)     clean -dxf
	git -C $(PROJ_SRC_DIR)     clean -dxf

.PHONY: cleanshort
cleanshort: ## Clean up go build artifacts and go proto-generated code.
cleanshort:
	rm -rf bin/.go_protobuf_sources bin/.gw_protobuf_sources
	-$(GO) clean $(GOFLAGS) -tags '$(TAGS)' -ldflags '$(LINKFLAGS)' -i github.com/cockroachdb/cockroach...
	$(FIND_RELEVANT) -type f -name '*.test' -exec rm {} +
	for f in cockroach*; do if [ -f "$$f" ]; then rm "$$f"; fi; done
	rm -rf $(ARCHIVE) pkg/sql/parser/gen

.PHONY: clean
clean: ## Like cleanshort, but also includes C++ artifacts, Bazel artifacts, and the go build cache.
clean: cleanshort clean-c-deps
	rm -rf build/defs.mk*
	-$(GO) clean $(GOFLAGS) $(GOMODVENDORFLAGS) -tags '$(TAGS)' -ldflags '$(LINKFLAGS)' -i -cache github.com/cockroachdb/cockroach...
	$(FIND_RELEVANT) -type f -name 'zcgo_flags*.go' -exec rm {} +
	if command -v bazel &> /dev/null; then bazel clean --expunge; fi
	rm -rf artifacts bin

.PHONY: maintainer-clean
maintainer-clean: ## Like clean, but also remove some auto-generated SQL parser, optgen, and UI protos code.
maintainer-clean: clean ui-maintainer-clean
	rm -f $(SQLPARSER_TARGETS) $(LOG_TARGETS) $(OPTGEN_TARGETS) $(UI_PROTOS_OSS)

.PHONY: unsafe-clean
unsafe-clean: ## Like maintainer-clean, but also remove all untracked/ignored files.
unsafe-clean: maintainer-clean unsafe-clean-c-deps
	git clean -dxf

# The following rules automatically generate dependency information for Go
# binaries. See [0] for details on the approach.
#
# [0]: http://make.mad-scientist.net/papers/advanced-auto-dependency-generation/

bins = \
  bin/allocsim \
  bin/benchmark \
  bin/cockroach-oss \
  bin/cockroach-short \
  bin/cockroach-sql \
  bin/docgen \
  bin/execgen \
  bin/fuzz \
  bin/generate-binary \
  bin/terraformgen \
  bin/gossipsim \
  bin/langgen \
  bin/protoc-gen-gogoroach \
  bin/publish-artifacts \
  bin/publish-provisional-artifacts \
  bin/optfmt \
  bin/optgen \
  bin/reduce \
  bin/returncheck \
  bin/roachvet \
  bin/roachprod \
  bin/roachprod-stress \
  bin/roachtest \
  bin/skip-test \
  bin/uptodate \
  bin/urlcheck \
  bin/zerosum

# `xbins` contains binaries that should be compiled for the target architecture
# (not the host), and should therefore be built with `xgo`.
xbins = \
  bin/workload

testbins = \
  bin/logictest \
  bin/logictestopt

# Mappings for binaries that don't live in pkg/cmd.
execgen-package = ./pkg/sql/colexec/execgen/cmd/execgen
langgen-package = ./pkg/sql/opt/optgen/cmd/langgen
optfmt-package = ./pkg/sql/opt/optgen/cmd/optfmt
optgen-package = ./pkg/sql/opt/optgen/cmd/optgen
logictest-package = ./pkg/sql/logictest
logictestopt-package = ./pkg/sql/opt/exec/execbuilder
terraformgen-package = ./pkg/roachprod/vm/aws/terraformgen
logictest-bins := bin/logictest bin/logictestopt

# Additional dependencies for binaries that depend on generated code.
#
# TODO(benesch): Derive this automatically. This is getting out of hand.
bin/workload bin/docgen bin/execgen bin/roachtest bin/roachvet $(logictest-bins): $(SQLPARSER_TARGETS) $(LOG_TARGETS) $(PROTOBUF_TARGETS)
bin/workload bin/docgen bin/roachtest $(logictest-bins): $(LIBPROJ) $(CGO_FLAGS_FILES)
bin/roachtest $(logictest-bins): $(C_LIBS_OSS) $(CGO_FLAGS_FILES) $(OPTGEN_TARGETS) | $(C_LIBS_DYNAMIC)

PREREQS := GOFLAGS= bin/prereqs

has-build-info = \
  bin/roachprod \
  bin/roachtest \
  bin/workload

$(has-build-info): override LINKFLAGS += \
	-X "github.com/cockroachdb/cockroach/pkg/build.tag=$(if $(BUILDINFO_TAG),$(BUILDINFO_TAG),$(shell cat .buildinfo/tag))" \
	-X "github.com/cockroachdb/cockroach/pkg/build.rev=$(shell cat .buildinfo/rev)" \
	-X "github.com/cockroachdb/cockroach/pkg/build.cgoTargetTriple=$(TARGET_TRIPLE)" \
	$(if $(BUILDCHANNEL),-X "github.com/cockroachdb/cockroach/pkg/build.channel=$(BUILDCHANNEL)")

$(bins): bin/%: bin/%.d | bin/prereqs
	@echo go install -v $*
	$(PREREQS) $(if $($*-package),$($*-package),./pkg/cmd/$*) > $@.d.tmp
	mv -f $@.d.tmp $@.d
	$(GO_INSTALL) -tags '$(TAGS)' -ldflags '$(LINKFLAGS)' -v $(if $($*-package),$($*-package),./pkg/cmd/$*)

$(xbins): bin/%: bin/%.d | bin/prereqs
	@echo go build -v $(GOFLAGS) $(GOMODVENDORFLAGS) -tags '$(TAGS)' -ldflags '$(LINKFLAGS)' -o $@ $*
	$(PREREQS) $(if $($*-package),$($*-package),./pkg/cmd/$*) > $@.d.tmp
	mv -f $@.d.tmp $@.d
	$(xgo) build -v $(GOFLAGS) $(GOMODVENDORFLAGS) -tags '$(TAGS)' -ldflags '$(LINKFLAGS)' -o $@ $(if $($*-package),$($*-package),./pkg/cmd/$*)

$(testbins): bin/%: bin/%.d | bin/prereqs
	@echo go test -c $($*-package)
	$(PREREQS) -bin-name=$* -test $($*-package) > $@.d.tmp
	mv -f $@.d.tmp $@.d
	$(xgo) test $(GOTESTFLAGS) $(GOFLAGS) $(GOMODVENDORFLAGS) -tags '$(TAGS)' -ldflags '$(LINKFLAGS)' -c -o $@ $($*-package)

bin/prereqs: ./pkg/cmd/prereqs/*.go vendor/modules.txt
	@echo go install -v ./pkg/cmd/prereqs
	@$(GO_INSTALL) -v ./pkg/cmd/prereqs

.PHONY: fuzz
fuzz: ## Run fuzz tests.
fuzz: bin/fuzz
	bin/fuzz $(TESTFLAGS) -tests $(TESTS) -timeout $(TESTTIMEOUT) $(PKG)

# Short hand to re-generate all bazel BUILD files. (Does the same thing as
# `./dev generate bazel`.)
.PHONY: bazel-generate
bazel-generate:
	@echo 'Generating DEPS.bzl and BUILD files using gazelle'
	./build/bazelutil/bazel-generate.sh

# No need to include all the dependency files if the user is just
# requesting help or cleanup.
ifneq ($(build-with-dep-files),)
.SECONDARY: bin/%.d
.PRECIOUS: bin/%.d
bin/%.d: ;

include $(wildcard bin/*.d)
endif

# Make doesn't expose a list of the variables declared in a given file, so we
# resort to sed magic. Roughly, this sed command prints VARIABLE in lines of the
# following forms:
#
#     [export] VARIABLE [:+?]=
#     TARGET-NAME: [export] VARIABLE [:+?]=
#
# The additional complexity below handles whitespace and comments.
#
# The special comments at the beginning are for GitHub/Go/Reviewable:
# https://github.com/golang/go/issues/13560#issuecomment-277804473
# https://github.com/Reviewable/Reviewable/wiki/FAQ#how-do-i-tell-reviewable-that-a-file-is-generated-and-should-not-be-reviewed
# Note how the 'prefix' variable is manually appended. This is required by Homebrew.
.SECONDARY: build/variables.mk
build/variables.mk: Makefile build/archive/contents/Makefile pkg/ui/Makefile build/defs.mk
	@echo '# Code generated by Make. DO NOT EDIT.' > $@.tmp
	@echo '# GENERATED FILE DO NOT EDIT' >> $@.tmp
	@echo 'define VALID_VARS' >> $@.tmp
	@sed -nE -e '/^	/d' -e 's/([^#]*)#.*/\1/' \
	  -e 's/(^|^[^:]+:)[ ]*(export)?[ ]*([[:upper:]_]+)[ ]*[:?+]?=.*/  \3/p' $^ \
	  | sort -u >> $@.tmp
	@echo '  prefix' >> $@.tmp
	@echo 'endef' >> $@.tmp
	@set -e; \
	if ! cmp -s $@.tmp $@; then \
	   mv -f $@.tmp $@; \
	else rm -f $@.tmp; fi

# Print an error if the user specified any variables on the command line that
# don't appear in this Makefile. The list of valid variables is automatically
# rebuilt on the first successful `make` invocation after the Makefile changes.
#
# TODO(peter): Figure out how to disallow overriding of variables that
# are not in the valid list from the environment. The problem is that
# any environment variable becomes a make variable and environments
# are dirty. For instance, my includes GREP_COLOR.
include build/variables.mk
$(foreach v,$(filter-out $(strip $(VALID_VARS)),$(.VARIABLES)),\
	$(if $(findstring command line,$(origin $v)),$(error Variable '$v' is not recognized by this Makefile)))

# Cypress e2e tests
.PHONY: db-console-e2e-test
db-console-e2e-test: pkg/ui/yarn.opt.installed
	cd pkg/ui/workspaces/db-console && yarn cypress:run
