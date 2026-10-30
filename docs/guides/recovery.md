# Disk encryption and recovery

This guide shows how to boot when the TPM does not unlock the disk, how to bind the disk to the TPM again, and how to check the encryption.

The root disk is encrypted with LUKS2, the standard Linux disk encryption. The TPM unlocks the disk at boot while the Secure Boot settings stay the same. The installer also makes a recovery key, which unlocks the disk when the TPM does not. See [Install to the disk](../getting-started/install.md#install-to-the-disk).

## When the TPM does not unlock the disk

A firmware update, a change to Secure Boot, or new Secure Boot keys can stop the TPM from unlocking the disk. The boot then asks for a passphrase.

1. Type the recovery key, with its dashes. If you set a disk passphrase at the install, you can type that instead.
2. After the boot, find the encrypted partition with `lsblk`.
3. Bind the disk to the TPM again, so that the next boot unlocks by itself. Replace `/dev/nvme0n1p3` with your partition:

   ```bash
   sudo systemd-cryptenroll --wipe-slot=tpm2 --tpm2-device=auto --tpm2-pcrs=7 /dev/nvme0n1p3
   ```

4. The command asks for the recovery key. Type it.

Without the recovery key and without a passphrase, the data on the disk is lost.

## Check the encryption

1. List the key slots of the encrypted partition. Replace `/dev/nvme0n1p3` with your partition:

   ```bash
   sudo systemd-cryptenroll /dev/nvme0n1p3
   ```

2. Make sure that the list shows a `tpm2` slot and a `recovery` slot.

## Model pools

A model pool is an extra disk for models. Each pool is also encrypted, and it has its own recovery key. See [Models](models.md) for pools.

## Protect the firmware

The TPM unlocks the disk for any system that boots through the same signed boot loader. A firmware password and a locked boot order are part of the protection. See [Prepare the computer](../getting-started/install.md#prepare-the-computer).
