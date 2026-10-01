#!/bin/sh
# gen-attest-keys.sh — generate the pci-attest operator key pair and put it in
# the repository's `release` environment, leaving no private key behind.
#
# Usage:
#   scripts/gen-attest-keys.sh [-R OWNER/REPO] [-o operator-pub.der] [-f]
#
# Sets two secrets of the GitHub environment `release`, the one the tag-only
# `debs` job of .github/workflows/build.yml runs in: ATTEST_SIGNING_KEY (the
# RSA private key, PEM) it signs the exporter with, and ATTEST_PUBLIC_KEY
# (base64 of the DER public key) it verifies against. Never repository
# secrets: any workflow pushed on any branch can read those.
#
# A missing environment is created locked down: only tags may deploy to it,
# and every run waits for your approval, so a writer who tags a commit with a
# doctored workflow still cannot reach the key unseen. An existing one must
# already require a reviewer, or nothing is uploaded.
#
# The public key is also saved (-o) for the hosts: copy it to each one and
# start QEMU with
#   -device pci-attest,pubkey=/etc/operator-pub.der
#
# The keys are generated in a private (0700) directory that is shredded and
# removed on every exit, failed or interrupted ones included. shred only erases
# what the filesystem overwrites in place, which a copy-on-write filesystem
# (APFS) or an SSD does not, so the directory lives in RAM where it can: a RAM
# disk on macOS, /dev/shm on Linux. Afterwards the private key exists only in
# the GitHub secret, which cannot be read back; losing it means rotating (-f).
#
# Needs openssl, base64 and gh (logged in, admin on the repository); shred
# when installed (gshred from Homebrew coreutils on macOS), otherwise the files
# are overwritten with dd the same way before they are removed.
set -eu

usage() {
	cat <<'EOF'
usage: scripts/gen-attest-keys.sh [-R OWNER/REPO] [-o PATH] [-f]

  -R, --repo OWNER/REPO  repository whose release environment gets the keys
                         (default: this checkout's GitHub repository)
  -o, --out PATH         where to save the public key for the hosts' QEMU
                         (default: ./operator-pub.der)
  -f, --force            rotate: replace secrets (and PATH) that already exist
EOF
}

# Must match `environment:` of the debs job in .github/workflows/build.yml.
ENV_NAME=release

die() {
	echo "error: $*" >&2
	exit 1
}

REPO=""
OUT="operator-pub.der"
FORCE=0
while [ $# -gt 0 ]; do
	case "$1" in
	-R | --repo)
		[ $# -ge 2 ] || die "$1 needs a value"
		REPO=$2
		shift 2
		;;
	-o | --out)
		[ $# -ge 2 ] || die "$1 needs a value"
		OUT=$2
		shift 2
		;;
	-f | --force)
		FORCE=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		usage >&2
		exit 2
		;;
	esac
done

for tool in openssl base64 gh; do
	command -v "$tool" >/dev/null 2>&1 || die "$tool not found"
done
SHRED=$(command -v shred || command -v gshred || true)
gh auth status >/dev/null 2>&1 || die "gh is not logged in: run gh auth login"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
if [ -z "$REPO" ]; then
	REPO=$(cd "$ROOT" && gh repo view --json nameWithOwner -q .nameWithOwner) ||
		die "cannot tell this checkout's GitHub repository; pass -R OWNER/REPO"
fi

# Read-only checks first, so that a refusal leaves GitHub as it was.

# A repository-level copy of the key is readable by any workflow on any
# branch, whatever the environment guards.
repo_secrets=$(gh secret list -R "$REPO" --json name -q '.[].name') ||
	die "cannot list the secrets of $REPO (admin access is needed to set them)"
if printf '%s\n' "$repo_secrets" | grep -qx ATTEST_SIGNING_KEY; then
	die "$REPO has ATTEST_SIGNING_KEY as a repository secret, which any workflow pushed \
on any branch can read: treat that key as exposed. Delete it, then rerun:
  gh secret delete ATTEST_SIGNING_KEY -R $REPO"
fi

ENV_API="repos/$REPO/environments/$ENV_NAME"
if env_out=$(gh api "$ENV_API" --jq .name 2>&1); then
	HAVE_ENV=1
else
	case "$env_out" in
	*"HTTP 404"*) HAVE_ENV=0 ;;
	*) die "cannot read the $ENV_NAME environment of $REPO: $env_out" ;;
	esac
fi

ROTATE=0
if [ "$HAVE_ENV" -eq 1 ]; then
	# The approval gate is what keeps the key from a doctored workflow; an
	# environment without it is not one to upload the key into.
	reviewers=$(gh api "$ENV_API" \
		--jq '[.protection_rules[]? | select(.type == "required_reviewers") | .reviewers[]?] | length')
	[ "$reviewers" -gt 0 ] || die "the $ENV_NAME environment of $REPO runs jobs without \
a reviewer's approval, so anyone who can push a tag could take the key. Add a required \
reviewer (Settings > Environments > $ENV_NAME) and rerun."
	# Tags only is the second line of defence: warn, do not refuse.
	open_to=$(gh api "$ENV_API" --jq '.deployment_branch_policy |
		if . == null then "every branch"
		elif .protected_branches then "protected branches"
		else "" end')
	if [ -z "$open_to" ]; then
		open_to=$(gh api "$ENV_API/deployment-branch-policies" \
			--jq '[.branch_policies[] | select((.type // "branch") != "tag") | .name] | join(", ")')
	fi
	[ -z "$open_to" ] || echo "warning: the $ENV_NAME environment also admits branches" \
		"($open_to); only tags need it" >&2

	# Replacing either secret rotates the operator key, which every host's
	# QEMU has to follow, so it is never done by accident.
	env_secrets=$(gh secret list -R "$REPO" --env "$ENV_NAME" --json name -q '.[].name') ||
		die "cannot list the secrets of the $ENV_NAME environment of $REPO"
	for name in ATTEST_SIGNING_KEY ATTEST_PUBLIC_KEY; do
		if printf '%s\n' "$env_secrets" | grep -qx "$name"; then
			[ "$FORCE" -eq 1 ] || die "the $ENV_NAME environment already has $name. Replacing \
it rotates the operator key: every host's pubkey must change with it, and exporters signed \
with the old key stop being accepted. Rerun with -f to do that."
			ROTATE=1
		fi
	done
fi
if [ -e "$OUT" ] && [ "$FORCE" -ne 1 ]; then
	die "$OUT exists; pass -f to replace it, or -o another path"
fi
if printf '%s\n' "$repo_secrets" | grep -qx ATTEST_PUBLIC_KEY; then
	echo "note: $REPO also has a repository secret ATTEST_PUBLIC_KEY; the release job" \
		"reads the environment's, so the old one can go: gh secret delete" \
		"ATTEST_PUBLIC_KEY -R $REPO" >&2
fi

# Writes start here.
if [ "$HAVE_ENV" -eq 0 ]; then
	me_id=$(gh api user --jq .id)
	me=$(gh api user --jq .login)
	gh api -X PUT "$ENV_API" --input - >/dev/null <<EOF
{
  "wait_timer": 0,
  "prevent_self_review": false,
  "reviewers": [{"type": "User", "id": $me_id}],
  "deployment_branch_policy": {"protected_branches": false, "custom_branch_policies": true}
}
EOF
	gh api -X POST "$ENV_API/deployment-branch-policies" -f name='*' -f type=tag >/dev/null
	echo "created the $ENV_NAME environment on $REPO: tags only, each run approved by $me" >&2
fi

umask 077
TMP="${TMPDIR:-/tmp}"
TMP="${TMP%/}"
KEYDIR=""
RAMDEV=""
MNT=""
OUT_TMP=""

shred_file() {
	if [ -n "$SHRED" ]; then
		"$SHRED" -u -z "$1"
	else
		size=$(wc -c <"$1" | tr -d ' ')
		if [ "$size" -gt 0 ]; then
			dd if=/dev/urandom of="$1" bs="$size" count=1 conv=notrunc 2>/dev/null
			dd if=/dev/zero of="$1" bs="$size" count=1 conv=notrunc 2>/dev/null
			sync
		fi
		rm -f "$1"
	fi
}

# Runs on every exit. Best effort throughout: one failing step must not leave
# the rest -- above all the shredding -- undone.
cleanup() {
	set +e
	if [ -n "$KEYDIR" ] && [ -d "$KEYDIR" ]; then
		for f in "$KEYDIR"/*; do
			[ -f "$f" ] && shred_file "$f"
		done
		rm -rf "$KEYDIR"
	fi
	[ -n "$OUT_TMP" ] && rm -f "$OUT_TMP"
	if [ -n "$RAMDEV" ]; then
		[ -n "$MNT" ] && diskutil unmount force "$MNT" >/dev/null 2>&1
		hdiutil detach -force "$RAMDEV" >/dev/null 2>&1 ||
			echo "warning: RAM disk $RAMDEV is still attached: hdiutil detach -force $RAMDEV" >&2
	fi
	[ -n "$MNT" ] && rmdir "$MNT" 2>/dev/null
	KEYDIR="" RAMDEV="" MNT="" OUT_TMP=""
	return 0
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# The private directory, in RAM where the platform allows.
case "$(uname -s)" in
Darwin)
	# A 4 MiB RAM disk, mounted inside the per-user $TMPDIR (0700), kept out
	# of Finder (nobrowse) and Spotlight (.metadata_never_index).
	if RAMDEV=$(hdiutil attach -nomount ram://8192 2>/dev/null | awk 'NR == 1 { print $1 }') &&
		[ -n "$RAMDEV" ] &&
		newfs_hfs -v attest-keys "$RAMDEV" >/dev/null 2>&1 &&
		MNT=$(mktemp -d "$TMP/attest-keys.XXXXXX") &&
		diskutil mount -mountOptions nobrowse -mountPoint "$MNT" "$RAMDEV" >/dev/null 2>&1; then
		touch "$MNT/.metadata_never_index"
		KEYDIR="$MNT/keys"
		mkdir "$KEYDIR"
	fi
	;;
Linux)
	if [ -d /dev/shm ] && [ -w /dev/shm ]; then
		KEYDIR=$(mktemp -d /dev/shm/attest-keys.XXXXXX)
	fi
	;;
esac
if [ -z "$KEYDIR" ]; then
	echo "warning: no RAM-backed directory here, so the keys go to disk, where shred" \
		"cannot guarantee they are gone" >&2
	KEYDIR=$(mktemp -d "$TMP/attest-keys.XXXXXX")
fi
chmod 700 "$KEYDIR"

echo "generating the operator key pair in $KEYDIR" >&2
# 2048 bits, as the QEMU tree's stand signs with: what the device is tested on.
openssl genrsa -out "$KEYDIR/operator.pem" 2048 || die "openssl genrsa failed"
openssl rsa -in "$KEYDIR/operator.pem" -pubout -RSAPublicKey_out -outform DER \
	-out "$KEYDIR/operator-pub.der" 2>/dev/null ||
	die "cannot derive the public key"

# Check the pair the way it will be used: sign as sign-elf.py does, verify
# with the public key loaded as QEMU loads it (DER RSAPublicKey).
printf 'pci-attest key check\n' >"$KEYDIR/probe"
openssl dgst -sha256 -sign "$KEYDIR/operator.pem" -out "$KEYDIR/probe.sig" "$KEYDIR/probe"
openssl rsa -RSAPublicKey_in -inform DER -in "$KEYDIR/operator-pub.der" \
	-pubout -out "$KEYDIR/pub.pem" 2>/dev/null ||
	die "the public key does not load the way QEMU loads it"
openssl dgst -sha256 -verify "$KEYDIR/pub.pem" -signature "$KEYDIR/probe.sig" \
	"$KEYDIR/probe" >/dev/null ||
	die "the generated key pair does not verify"

# The public key is staged next to -o before the private one is uploaded, and
# only put in place once that upload succeeds: a failed run leaves no public
# key matching nothing, and a half-done one leaves the key to finish it with.
OUT_TMP="$OUT.tmp.$$"
cp "$KEYDIR/operator-pub.der" "$OUT_TMP"
chmod 644 "$OUT_TMP"
gh secret set ATTEST_SIGNING_KEY -R "$REPO" --env "$ENV_NAME" <"$KEYDIR/operator.pem" ||
	die "setting ATTEST_SIGNING_KEY in the $ENV_NAME environment of $REPO failed; no key was changed"
mv -f "$OUT_TMP" "$OUT"
OUT_TMP=""
base64 <"$KEYDIR/operator-pub.der" | gh secret set ATTEST_PUBLIC_KEY -R "$REPO" --env "$ENV_NAME" ||
	die "ATTEST_SIGNING_KEY is set but ATTEST_PUBLIC_KEY is not; finish with:
  base64 < $OUT | gh secret set ATTEST_PUBLIC_KEY -R $REPO --env $ENV_NAME"

cleanup
trap - EXIT

fingerprint=$(openssl dgst -sha256 "$OUT" | awk '{ print $NF }')
OUT="$(cd "$(dirname "$OUT")" && pwd)/$(basename "$OUT")"
cat <<EOF
Set ATTEST_SIGNING_KEY and ATTEST_PUBLIC_KEY in the $ENV_NAME environment of
$REPO; the local copy of the private key is shredded, so it now exists only in
the GitHub secret. Each tag's release run waits for approval in the Actions tab.

Public key for the hosts: $OUT (sha256 $fingerprint)
  copy it to every host and start QEMU with
  -device pci-attest,pubkey=/etc/operator-pub.der
EOF
if [ "$ROTATE" -eq 1 ]; then
	echo "This was a rotation: until every host has the new public key, exporters" \
		"signed with it are refused there, and the old ones are refused where it is."
fi
