#!/bin/sh
# Prepare a self-hosted Firecracker sandbox on Linux with /dev/kvm.
# Downloads a Firecracker binary, a kernel and Ubuntu rootfs from the
# Firecracker CI, adds a local SSH key, and builds an ext4 rootfs.
# Run from anywhere; files go to .eipagent/firecracker.
set -eu
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
DIR="${FC_DIR:-$REPO_ROOT/.eipagent/firecracker}"
OWNER="${SUDO_USER:-$(id -un)}"
SUDO=""
if [ "$(id -u)" -ne 0 ]; then SUDO="sudo -n"; fi
mkdir -p "$DIR"

if [ ! -e /dev/kvm ]; then
	echo "no /dev/kvm: run this on Linux with KVM (on macOS, use Lima with nested virtualisation)" >&2
	exit 1
fi
if ! command -v unsquashfs >/dev/null 2>&1; then
	$SUDO apt-get update -y
	$SUDO apt-get install -y squashfs-tools
fi

ARCH=$(uname -m)
S3="https://s3.amazonaws.com/spec.ccfc.min"
echo "finding the latest Firecracker CI kernel and rootfs for $ARCH..."
CI_ARTIFACTS_PREFIX=$(curl -fsSL "$S3?list-type=2&prefix=firecracker-ci/&delimiter=/" \
	| grep -oP "(?<=<Prefix>)firecracker-ci/[0-9]{8}-[^/]+/(?=</Prefix>)" | sort | tail -1)
latest_kernel_key=$(curl -fsSL "$S3?list-type=2&prefix=${CI_ARTIFACTS_PREFIX}${ARCH}/vmlinux-" \
	| grep -oP "(?<=<Key>)(${CI_ARTIFACTS_PREFIX}${ARCH}/vmlinux-[0-9]+\.[0-9]+\.[0-9]{1,3})(?=</Key>)" | sort -V | tail -1)
latest_ubuntu_key=$(curl -fsSL "$S3?list-type=2&prefix=${CI_ARTIFACTS_PREFIX}${ARCH}/ubuntu-" \
	| grep -oP "(?<=<Key>)(${CI_ARTIFACTS_PREFIX}${ARCH}/ubuntu-[0-9]+\.[0-9]+\.squashfs)(?=</Key>)" | sort -V | tail -1)
[ -n "$latest_kernel_key" ] && [ -n "$latest_ubuntu_key" ] || { echo "could not find Firecracker CI artifacts" >&2; exit 1; }

echo "downloading kernel..."
curl -fsSL "$S3/$latest_kernel_key" -o "$DIR/vmlinux"
echo "downloading rootfs..."
curl -fsSL "$S3/$latest_ubuntu_key" -o "$DIR/ubuntu.squashfs"

echo "creating SSH key..."
if [ ! -f "$DIR/id_ed25519" ]; then
	ssh-keygen -q -t ed25519 -N "" -f "$DIR/id_ed25519"
fi
rm -rf "$DIR/squashfs-root"
unsquashfs -q -f -d "$DIR/squashfs-root" "$DIR/ubuntu.squashfs"
mkdir -p "$DIR/squashfs-root/root/.ssh"
cat "$DIR/id_ed25519.pub" >> "$DIR/squashfs-root/root/.ssh/authorized_keys"
chmod 700 "$DIR/squashfs-root/root/.ssh"
chmod 600 "$DIR/squashfs-root/root/.ssh/authorized_keys"
$SUDO chown -R root:root "$DIR/squashfs-root"

echo "building rootfs.ext4 (1 GiB)..."
rm -f "$DIR/rootfs.ext4"
truncate -s 1G "$DIR/rootfs.ext4"
$SUDO mkfs.ext4 -q -d "$DIR/squashfs-root" -F "$DIR/rootfs.ext4"

echo "downloading Firecracker..."
release_url="https://github.com/firecracker-microvm/firecracker/releases"
latest=$(basename "$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$release_url/latest")")
curl -fsSL "$release_url/download/$latest/firecracker-$latest-$ARCH.tgz" -o "$DIR/firecracker.tgz"
rm -rf "$DIR/release"
mkdir -p "$DIR/release"
tar -xzf "$DIR/firecracker.tgz" -C "$DIR/release"
BIN=$(find "$DIR/release" -type f -name "firecracker-$latest-$ARCH" | head -1)
[ -n "$BIN" ] || { echo "could not find the firecracker binary" >&2; exit 1; }
$SUDO install -m 0755 "$BIN" /usr/local/bin/firecracker

chmod 600 "$DIR/id_ed25519"
if [ "$(id -u)" -eq 0 ] && [ "$OWNER" != "root" ]; then
	chown -R "$OWNER" "$DIR"
fi
echo
echo "ready. files are in $DIR"
echo "  firecracker: /usr/local/bin/firecracker"
echo "  kernel:      $DIR/vmlinux"
echo "  rootfs:      $DIR/rootfs.ext4"
echo "  ssh key:     $DIR/id_ed25519"
echo
echo "run the example from the repo root:"
echo "  go run ./examples/firecracker"
echo
echo "the example uses sudo -n for the tap device and the Firecracker process."
