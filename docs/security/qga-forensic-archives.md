# QGA forensic archives

Use a dedicated volume for forensic archives and a private directory for panel keys. Create both directories with mode `0700`, owned by the account running the panel. Use a separate volume with an enforced capacity and retain the archives according to the incident's needs.

Run `komari init-forensic-keys /path/to/private-keys` once on the panel. It prints the X25519 recipient public key and the Ed25519 receipt public key and creates two mode `0600` key files. Copy the public keys to the node operator through a verified administrative channel. Keep both private keys on the panel host. Keep an independent copy of the receipt public key with incident documentation.

Set `KOMARI_PANEL_DIAGNOSTIC_NODE` to the UUID of the enrolled node allowed to submit to this archive, `KOMARI_PANEL_DIAGNOSTIC_ARCHIVE` to the absolute archive directory, and `KOMARI_PANEL_DIAGNOSTIC_SIGNING_KEY` to the generated `forensic-receipt.key` file. The panel signs a receipt for each stored record and keeps the encrypted originals as `.tar.gz` files. The first version lists up to 1024 directory entries. Move closed sessions into separate protected retention storage before the active directory exceeds that limit. Preserve the appropriate public key when changing archive or signing keys.

Open **Forensic archives** in the administration interface. Download requires fresh verification for that archive. The file contains the encrypted record, a manifest and the signed receipt. To verify the original using the documented receipt key, run

```sh
komari verify-forensic-archive --receipt-public-key "$RECEIPT_PUBLIC_KEY" ORIGINAL.tar.gz
```

To disclose the record, run verification on the panel host with its X25519 private key and a new output filename inside a private directory.

```sh
komari verify-forensic-archive \
  --receipt-public-key "$RECEIPT_PUBLIC_KEY" \
  --decryption-key-file /path/to/private-keys/forensic-recipient.key \
  --record-output /path/to/private-disclosure/observed-record.json \
  ORIGINAL.tar.gz
```

Keep the encrypted original and record the circumstances of collection separately. The signed reception time and ciphertext digest verify what the panel received. Guest observations show QGA messages seen from inside the guest. They provide no direct observation of who operated the hypervisor or of accesses that bypassed the QGA channel.
