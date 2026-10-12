.PHONY: all activity test-activity install-activity dolphin test-dolphin install-dolphin test test-race test-live test-init srpm rpm dsc changes deb clean install uninstall

# autocalculate software/package versions
VERSION := $(shell grep Version onedriver.spec | sed 's/Version: *//g')
RELEASE := $(shell grep -oP "Release: *[0-9]+" onedriver.spec | sed 's/Release: *//g')
DIST := $(shell rpm --eval "%{?dist}" 2> /dev/null || echo 1)
RPM_FULL_VERSION = $(VERSION)-$(RELEASE)$(DIST)

# -Wno-deprecated-declarations is for gotk3, which uses deprecated methods for older
# glib compatibility: https://github.com/gotk3/gotk3/issues/762#issuecomment-919035313
CGO_CFLAGS := CGO_CFLAGS=-Wno-deprecated-declarations

# install locations, DESTDIR is for package builds
PREFIX ?= /usr
SYSCONFDIR ?= /etc
SYSTEMDUSERDIR ?= /etc/systemd/user

# install locations, DESTDIR is for package builds
PREFIX ?= /usr
SYSCONFDIR ?= /etc
SYSTEMDUSERDIR ?= /etc/systemd/user

# test-specific variables
TEST_UID := $(shell whoami)
GORACE := GORACE="log_path=fusefs_tests.race strip_path_prefix=1"

all: onedriver onedriver-launcher onedriver-tray


onedriver: $(shell find fs/ -type f) cmd/onedriver/main.go
	bash cgo-helper.sh 
	$(CGO_CFLAGS) go build -v \
		-ldflags="-X github.com/jstaf/onedriver/cmd/common.commit=$(shell git rev-parse HEAD)" \
		./cmd/onedriver


onedriver-headless: $(shell find fs/ cmd/common/ -type f) cmd/onedriver/main.go
	CGO_ENABLED=0 go build -v -o onedriver-headless \
		-ldflags="-X github.com/jstaf/onedriver/cmd/common.commit=$(shell git rev-parse HEAD)" \
		./cmd/onedriver


onedriver-launcher: $(shell find ui/ cmd/common/ -type f) cmd/onedriver-launcher/main.go
	$(CGO_CFLAGS) go build -v \
		-ldflags="-X github.com/jstaf/onedriver/cmd/common.commit=$(shell git rev-parse HEAD)" \
		./cmd/onedriver-launcher


# the tray icon is pure Go (no cgo, no GTK)
onedriver-tray: $(shell find cmd/onedriver-tray/ -type f)
	CGO_ENABLED=0 go build -v ./cmd/onedriver-tray


# The activity popup (Qt6/QML/Kirigami) needs cmake and Qt, so it is not part of
# "all". Build it with "make activity", see "Activity window" in the README.
activity:
	cmake -S activity -B build/activity -DBUILD_TESTING=ON $(CMAKE_FLAGS)
	cmake --build build/activity


test-activity: activity
	ctest --test-dir build/activity --output-on-failure


install-activity: activity
	DESTDIR=$(DESTDIR) cmake --install build/activity --prefix $(PREFIX)


# The Dolphin integration (KF6 overlay icons + context menu), also not part of
# "all". See "Dolphin integration" in the README.
dolphin:
	cmake -S dolphin -B build/dolphin -DBUILD_TESTING=ON $(CMAKE_FLAGS)
	cmake --build build/dolphin


test-dolphin: dolphin
	ctest --test-dir build/dolphin --output-on-failure


install-dolphin: dolphin
	DESTDIR=$(DESTDIR) cmake --install build/dolphin --prefix $(PREFIX)


install: onedriver onedriver-launcher onedriver-tray
	install -Dm755 onedriver $(DESTDIR)$(PREFIX)/bin/onedriver
	install -Dm755 onedriver-launcher $(DESTDIR)$(PREFIX)/bin/onedriver-launcher
	install -Dm755 onedriver-tray $(DESTDIR)$(PREFIX)/bin/onedriver-tray
	install -Dm644 pkg/resources/onedriver.svg $(DESTDIR)$(PREFIX)/share/icons/onedriver/onedriver.svg
	install -Dm644 pkg/resources/onedriver.png $(DESTDIR)$(PREFIX)/share/icons/onedriver/onedriver.png
	install -Dm644 pkg/resources/onedriver-128.png $(DESTDIR)$(PREFIX)/share/icons/onedriver/onedriver-128.png
	install -Dm644 pkg/resources/onedriver-launcher.desktop $(DESTDIR)$(PREFIX)/share/applications/onedriver-launcher.desktop
	install -Dm644 pkg/resources/onedriver-tray.desktop $(DESTDIR)$(PREFIX)/share/applications/onedriver-tray.desktop
	install -Dm644 pkg/resources/onedriver-tray.desktop $(DESTDIR)$(SYSCONFDIR)/xdg/autostart/onedriver-tray.desktop
	install -Dm644 pkg/resources/onedriver@.service $(DESTDIR)$(SYSTEMDUSERDIR)/onedriver@.service
	mkdir -p $(DESTDIR)$(PREFIX)/share/man/man1
	gzip -9nc pkg/resources/onedriver.1 > $(DESTDIR)$(PREFIX)/share/man/man1/onedriver.1.gz
	$(if $(DESTDIR),,mandb)


uninstall:
	rm -f \
		$(PREFIX)/bin/onedriver \
		$(PREFIX)/bin/onedriver-launcher \
		$(PREFIX)/bin/onedriver-tray \
		$(PREFIX)/bin/onedriver-activity \
		$(PREFIX)/share/applications/onedriver-activity.desktop \
		$(SYSCONFDIR)/xdg/autostart/onedriver-tray.desktop \
		$(PREFIX)/share/applications/onedriver-tray.desktop \
		$(SYSTEMDUSERDIR)/onedriver@.service \
		$(PREFIX)/share/applications/onedriver-launcher.desktop \
		$(PREFIX)/share/man/man1/onedriver.1.gz
	rm -rf $(PREFIX)/share/icons/onedriver
	$(if $(DESTDIR),,mandb)


# used to create release tarball for rpmbuild
v$(VERSION).tar.gz: $(shell git ls-files)
	rm -rf onedriver-$(VERSION)
	mkdir -p onedriver-$(VERSION)
	git ls-files > filelist.txt
	git rev-parse HEAD > .commit
	echo .commit >> filelist.txt
	rsync -a --files-from=filelist.txt . onedriver-$(VERSION)
	mv onedriver-$(VERSION)/pkg/debian onedriver-$(VERSION)
	go mod vendor
	cp -R vendor/ onedriver-$(VERSION)
	tar -czf $@ onedriver-$(VERSION)


# build srpm package used for rpm build with mock
srpm: onedriver-$(RPM_FULL_VERSION).src.rpm 
onedriver-$(RPM_FULL_VERSION).src.rpm: v$(VERSION).tar.gz
	rpmbuild -ts $<
	cp $$(rpm --eval '%{_topdir}')/SRPMS/$@ .


# build the rpm for the default mock target
MOCK_CONFIG=$(shell readlink -f /etc/mock/default.cfg | grep -oP '[a-z0-9-]+x86_64')
rpm: onedriver-$(RPM_FULL_VERSION).x86_64.rpm
onedriver-$(RPM_FULL_VERSION).x86_64.rpm: onedriver-$(RPM_FULL_VERSION).src.rpm
	mock -r /etc/mock/$(MOCK_CONFIG).cfg $<
	cp /var/lib/mock/$(MOCK_CONFIG)/result/$@ .


# create a release tarball for debian builds
onedriver_$(VERSION).orig.tar.gz: v$(VERSION).tar.gz
	cp $< $@


# create the debian source package for the current version
changes: onedriver_$(VERSION)-$(RELEASE)_source.changes
onedriver_$(VERSION)-$(RELEASE)_source.changes: onedriver_$(VERSION).orig.tar.gz
	cd onedriver-$(VERSION) && debuild -S -sa -d


# just a helper target to use while building debs
dsc: onedriver_$(VERSION)-$(RELEASE).dsc
onedriver_$(VERSION)-$(RELEASE).dsc: onedriver_$(VERSION).orig.tar.gz
	dpkg-source --build onedriver-$(VERSION)


# create the debian package in a chroot via pbuilder
deb: onedriver_$(VERSION)-$(RELEASE)_amd64.deb
onedriver_$(VERSION)-$(RELEASE)_amd64.deb: onedriver_$(VERSION)-$(RELEASE).dsc
	sudo mkdir -p /var/cache/pbuilder/aptcache
	sudo pbuilder --build $<
	cp /var/cache/pbuilder/result/$@ .


# a large text file for us to test upload sessions with. #science
dmel.fa:
	curl ftp://ftp.ensemblgenomes.org/pub/metazoa/release-42/fasta/drosophila_melanogaster/dna/Drosophila_melanogaster.BDGP6.22.dna.chromosome.X.fa.gz | zcat > $@


# setup tests for the first time on a new computer
test-init: onedriver
	go install github.com/rakyll/gotest@latest
	mkdir -p mount/
	$< -a mount/	


# Runs the test suite against an in-memory mock of Microsoft Graph. This needs
# no OneDrive account and no network access, only FUSE (fusermount3) and bash,
# and is what CI runs. Tests that can only work against the real service are
# skipped. See "Running tests" in the README.
test:
	$(CGO_CFLAGS) go test -count=1 -timeout 300s ./fs/... ./cmd/onedriver-tray/...


# Same as "make test", but with the race detector enabled.
test-race:
	$(CGO_CFLAGS) $(GORACE) go test -race -count=1 -timeout 600s ./fs/...


# Runs the full suite against a real OneDrive account (set up with "make
# test-init" first). For offline tests, the test binary is built online, then
# network access is disabled and tests are run. sudo is required - otherwise we
# don't have permission to deny network access to onedriver during the test.
test-live: onedriver onedriver-launcher dmel.fa
	rm -f *.race* fusefs_tests.log
	CGO_ENABLED=0 ONEDRIVER_TEST_LIVE=1 gotest -v -parallel=8 -count=1 $(shell go list ./ui/... | grep -v offline)
	$(CGO_CFLAGS) ONEDRIVER_TEST_LIVE=1 gotest -v -parallel=8 -count=1 ./cmd/...
	$(CGO_CFLAGS) $(GORACE) ONEDRIVER_TEST_LIVE=1 gotest -race -v -parallel=8 -count=1 ./fs/graph/...
	$(CGO_CFLAGS) $(GORACE) ONEDRIVER_TEST_LIVE=1 gotest -race -v -parallel=8 -count=1 ./fs
	$(CGO_CFLAGS) go test -c ./fs/offline
	@echo "sudo is required to run tests of offline functionality:"
	sudo unshare -n sudo -u $(TEST_UID) env ONEDRIVER_TEST_LIVE=1 ./offline.test -test.v -test.parallel=8 -test.count=1


# will literally purge everything: all built artifacts, all logs, all tests,
# all files tests depend on, all auth tokens... EVERYTHING
clean:
	fusermount3 -uz mount/ || true
	rm -f *.db *.rpm *.deb *.dsc *.changes *.build* *.upload *.xz filelist.txt .commit
	rm -f *.log *.fa *.gz *.test vgcore.* onedriver onedriver-headless onedriver-launcher onedriver-tray .auth_tokens.json
	rm -rf util-linux-*/ onedriver-*/ vendor/ build/
