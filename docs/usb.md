# Bootable autoinstall USB

`scripts/make-usb.sh` builds a USB stick that installs Ubuntu Server 24.04
unattended, partitions the SSD (root) and HDD (backups) per the design,
installs and enables `oakd` and the backup timer, and reboots into a working
host. The only manual step left afterward is `cloudflared tunnel login`.

## Required tools (on the Mac)

```bash
brew install xorriso coreutils gettext
```

`gettext` provides `envsubst`, used to render `usb/user-data.tmpl`.

## Required environment variables

| Var | Example | Notes |
|---|---|---|
| `OAK_HOSTNAME` | `oak` | box hostname |
| `OAK_SSH_KEY` | `$(cat ~/.ssh/id_ed25519.pub)` | one public key line, quoted |
| `OAK_DOMAIN` | `apps.example.com` | apps domain (see docs/host.md) |
| `OAK_SSD` | `/dev/nvme0n1` | install target: OS + `/var/lib/oak` |
| `OAK_HDD` | `/dev/sdb` | backups target: `/var/lib/oak/backups` |
| `OAK_PASSWORD_HASH` | `$(openssl passwd -6)` | hashed password for the `oak` user |

Optional: `OAK_UBUNTU_VERSION` (default `24.04.1`), `DEV` (a macOS disk
identifier -- see below; the script never touches a disk unless this is
set).

## Finding the target's disk paths

`OAK_SSD`/`OAK_HDD` are Linux device paths *on the box being installed*, not
on the Mac. If you don't already know them:

1. Boot the target once from a stock Ubuntu Server ISO (or any Linux live
   USB) and run `lsblk` to see `/dev/nvme0n1`, `/dev/sda`, etc.
2. Or, since this is a fresh box: NVMe drives are almost always
   `/dev/nvme0n1`; the one spinning HDD is almost always `/dev/sda`.

## Building the ISO

```bash
export OAK_HOSTNAME=oak
export OAK_SSH_KEY="$(cat ~/.ssh/id_ed25519.pub)"
export OAK_DOMAIN=apps.example.com
export OAK_SSD=/dev/nvme0n1
export OAK_HDD=/dev/sda
export OAK_PASSWORD_HASH="$(openssl passwd -6)"

make usb
```

This downloads the Ubuntu ISO into `build/` (skipped if already present),
verifies its SHA256 against the upstream `SHA256SUMS`, builds `oakd` and
`oak-init`, stages them plus `host-setup.sh`, `backup.sh`, the systemd units
and a freshly generated `oakd.toml` (with a random `api_token`, printed at
the end -- save it) under `build/oak/`, renders `usb/user-data.tmpl`, and
repacks the ISO as `build/oak-install.iso`.

**Verify before writing to a stick:** the script's grub.cfg patch targets
Ubuntu 24.04's standard live-server menu entry; it hasn't been run
end-to-end in this environment (no ISO download or xorriso invocation was
performed while writing it -- see the `ponytail:` comment in the script). Before writing to a real USB stick, check
`build/iso-work/grub.cfg` actually gained
` autoinstall ds=nocloud\;s=/cdrom/autoinstall/` on its `linux
/casper/vmlinuz` line.

## Writing the stick

Find the disk identifier for the USB stick itself (on the Mac):

```bash
diskutil list
```

Then either let `make usb` write it directly:

```bash
DEV=/dev/diskN make usb   # re-running is safe; the ISO build is cached
```

or write the already-built ISO by hand:

```bash
diskutil unmountDisk /dev/diskN
sudo dd if=build/oak-install.iso of=/dev/rdiskN bs=4m status=progress
diskutil eject /dev/diskN
```

`DEV` (and the `dd` target above) must be the **whole disk** (`diskN`), not
a partition (`diskNsM`) -- `dd`-ing an ISO to a partition device does not
produce a bootable stick. Double check with `diskutil list` before writing;
this is destructive to whatever is on that disk.

## Installing the box

1. Enable **SVM** (AMD) or **VT-x/VMX** (Intel) in BIOS/UEFI -- required for
   Firecracker later (see docs/host.md #1).
2. Boot from the USB stick in UEFI mode.
3. The install is unattended: expect ~10 minutes and one reboot. It
   partitions `OAK_SSD` (EFI + root, mounted at `/`) and `OAK_HDD`
   (mounted at `/var/lib/oak/backups`), runs `host-setup.sh`, and installs
   `oakd`/`oak-init`/`oak-backup.sh` plus their systemd units, enabled.

## After the reboot

```bash
ssh oak@<box-ip>
sudo cloudflared tunnel login
sudo cloudflared tunnel create oak
```

Then, as in docs/host.md's steps 3-5: write `/etc/cloudflared/config.yml`
with the printed tunnel ID and credentials path, `cloudflared service
install && systemctl enable --now cloudflared`, add the tunnel ID to
`/etc/oak/oakd.toml`'s `tunnel_id`, add the wildcard DNS CNAME, and
`sudo systemctl restart oakd`. From there, `scripts/smoke.sh` should pass.

## Skipped (v1)

Custom minimal distro, netboot, Secure Boot signing. Add these if a second
box makes reinstalls frequent enough to matter.
