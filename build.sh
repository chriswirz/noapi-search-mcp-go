#!/bin/sh
# Build noapi-search-mcp.
#
# Formats, vets, tests and builds. By default it builds for this machine, which
# is what you want while working; --all cross-compiles every released target,
# which is what the release pipeline does.
#
# The version is stamped in with -ldflags so `--version` reports something
# meaningful rather than "dev". Without --version it is derived from git: the
# tag if the commit has one, otherwise the short commit with -dirty when the
# tree has uncommitted changes, so a binary can be traced back to what built it.
#
#   ./build.sh                     build for this machine
#   ./build.sh --all               cross-compile every released target
#   ./build.sh --all --version 1.2.0
#   ./build.sh --skip-tests        rebuild quickly, no format/vet/test
#   ./build.sh --selftest          then check the live services still match
#   ./build.sh --clean             remove dist/ and the binaries first
#
# POSIX sh on purpose: this has to run on whatever shell the machine has.
set -eu

APP="noapi-search-mcp"
cd "$(dirname "$0")"

ALL=0
SKIP_TESTS=0
SELFTEST=0
CLEAN=0
VERSION=""

die() { printf 'error: %s\n' "$*" >&2; exit 1; }
step() { printf '\n==> %s\n' "$*"; }

while [ $# -gt 0 ]; do
	case "$1" in
	--all) ALL=1 ;;
	--skip-tests) SKIP_TESTS=1 ;;
	--selftest) SELFTEST=1 ;;
	--clean) CLEAN=1 ;;
	--version)
		[ $# -ge 2 ] || die "--version needs a value"
		VERSION="$2"
		shift
		;;
	--version=*) VERSION="${1#--version=}" ;;
	-h | --help)
		sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*) die "unknown option $1 (try --help)" ;;
	esac
	shift
done

command -v go >/dev/null 2>&1 || die "Go is not installed, or not on PATH. Install it from https://go.dev/dl/"

# resolve_version derives a version from git when one was not given.
resolve_version() {
	[ -n "$VERSION" ] && { printf '%s' "$VERSION"; return; }
	command -v git >/dev/null 2>&1 || { printf 'dev'; return; }
	git rev-parse --git-dir >/dev/null 2>&1 || { printf 'dev'; return; }

	if tag="$(git describe --tags --exact-match 2>/dev/null)"; then
		printf '%s' "${tag#v}"
		return
	fi
	commit="$(git rev-parse --short HEAD 2>/dev/null)" || { printf 'dev'; return; }
	suffix=""
	[ -n "$(git status --porcelain 2>/dev/null)" ] && suffix="-dirty"
	printf 'dev-%s%s' "$commit" "$suffix"
}

RESOLVED="$(resolve_version)"
printf '%s build\n' "$APP"
printf '  version   %s\n' "$RESOLVED"
printf '  go        %s\n' "$(go version | sed 's/^go version //')"

if [ "$CLEAN" -eq 1 ]; then
	step "Cleaning"
	rm -rf dist
	rm -f "$APP" "$APP.exe"
	echo "  removed dist/ and any built binaries"
fi

if [ "$SKIP_TESTS" -eq 0 ]; then
	step "Checking formatting"
	# The embedded Swagger UI assets are third-party and not Go, so they are
	# not gofmt's business.
	unformatted="$(gofmt -l . | grep -v '^static' || true)"
	if [ -n "$unformatted" ]; then
		printf '%s\n' "$unformatted" | sed 's/^/  needs gofmt: /'
		die "these files are not formatted. Run: gofmt -w ."
	fi
	echo "  all files are formatted"

	step "Vetting"
	go vet ./...

	step "Testing"
	go test ./...
else
	printf '\n  skipping format, vet and tests (--skip-tests)\n'
fi

# -trimpath keeps absolute build paths out of the binary; -s -w drop the symbol
# and DWARF tables, which is most of the size.
LDFLAGS="-s -w -X main.version=$RESOLVED"

# size prints a file's size in megabytes, using whichever stat this system has.
size() {
	bytes="$(wc -c <"$1" | tr -d ' ')"
	awk -v b="$bytes" 'BEGIN { printf "%.1f MB", b / 1048576 }'
}

if [ "$ALL" -eq 1 ]; then
	mkdir -p dist
	# CGO off, so each binary is a single static file that runs on any machine
	# of its architecture without a matching libc.
	for target in windows/amd64/.exe windows/arm64/.exe linux/amd64/ linux/arm64/; do
		os="$(echo "$target" | cut -d/ -f1)"
		arch="$(echo "$target" | cut -d/ -f2)"
		ext="$(echo "$target" | cut -d/ -f3)"
		out="dist/$APP-$os-$arch$ext"

		step "Building $os/$arch"
		GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 \
			go build -trimpath -ldflags "$LDFLAGS" -o "$out" .
		printf '  %s  (%s)\n' "$out" "$(size "$out")"
	done

	step "Checksums"
	# Written from inside dist so the names in the file are bare, which is what
	# `sha256sum -c SHA256SUMS` expects when run from that directory.
	(
		cd dist
		if command -v sha256sum >/dev/null 2>&1; then
			sha256sum -- * >SHA256SUMS.tmp
		elif command -v shasum >/dev/null 2>&1; then
			shasum -a 256 -- * >SHA256SUMS.tmp
		else
			rm -f SHA256SUMS.tmp
			printf '  no sha256sum or shasum available; skipping checksums\n' >&2
			exit 0
		fi
		grep -v 'SHA256SUMS' SHA256SUMS.tmp >SHA256SUMS
		rm -f SHA256SUMS.tmp
		sed 's/^/  /' SHA256SUMS
	)
else
	out="$APP"
	step "Building for this machine"
	go build -trimpath -ldflags "$LDFLAGS" -o "$out" .
	printf '  %s  (%s)\n' "$out" "$(size "$out")"

	step "Verifying"
	"./$out" --version

	if [ "$SELFTEST" -eq 1 ]; then
		step "Compatibility checks against the live services"
		echo "  This makes real requests and takes a minute or two."
		# A broken check means a service changed its markup, which is worth
		# failing the build over: the binary compiles and does not work. Rate
		# limiting exits zero and does not reach here.
		"./$out" --selftest
	fi
fi

printf '\nBuild succeeded.\n'
if [ "$ALL" -eq 0 ]; then
	printf '  Try:  ./%s --transport http     then open http://127.0.0.1:8780/docs\n' "$APP"
fi
