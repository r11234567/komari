# Temporary inbound SSH on a Linux node

[简体中文](temporary-ssh.zh-CN.md)

Temporary SSH admits one source IPv4 or IPv6 address to a fixed SSH port for 15 minutes. The node needs an updated Agent, an authorized rescue helper and an available nftables entry point. Browser WebSSH remains a separate option.

## Configure the node

The first rescue helper installation confirmed by `komari-host apply` automatically prepares the entry point on compatible nftables hosts using the same local PAM confirmation. Each subsequent grant is requested from the panel with two-factor verification.

For an existing rescue helper without an entry point, or if setup asks you to select an interface or port, run the following command from an SSH session you opened directly. The command keeps a working configuration already on the node.

```bash
sudo komari-host setup-rescue-ssh --manifest /etc/komari/services/komari-agent.json
```

The command verifies the SSH listener, default interface and native nftables input hook, then persists the empty timeout sets and rules. When preparing Rescue with the Agent installer, pass `--install-rescue --install-setup-rescue-ssh` to run this step. If several candidates exist, use `--port`, `--interface`, `--table` and `--chain` to identify the intended entry point. With a custom firewall loader, `--nft-config` can identify the persistent entry file loaded on boot. For UFW, firewalld and legacy iptables, setup identifies the active firewall and directs the administrator to a native nftables entry point or WebSSH.

### Manual configuration

Create the root-owned file `/etc/komari/services/komari-agent.temporary-ssh.json` with mode `0600`. Replace `komari-agent` in the filename when the Agent runs under a custom service name. This example uses the public interface `eth0` and an SSH listener on TCP `8658`.

```json
{
  "table": "public_edge_guard",
  "chain": "input",
  "interface": "eth0",
  "port": 8658
}
```

Add these empty sets inside the existing `table inet public_edge_guard` in the host's persistent firewall configuration.

```nft
set komari_rescue_ssh_v4 {
    type ipv4_addr
    flags timeout
}
set komari_rescue_ssh_v6 {
    type ipv6_addr
    flags timeout
}
```

Add these rules in the existing `input` filter chain immediately before the `eth0` catch-all drop. Keep the expression order and use the interface and port from the JSON file. Both sets and rules are required. Reference these dedicated sets only from the corresponding rules below. Keep IPv6 neighbor discovery working on the public interface if the host accepts IPv6 connections.

```nft
iifname "eth0" ip saddr @komari_rescue_ssh_v4 tcp dport 8658 accept
iifname "eth0" ip6 saddr @komari_rescue_ssh_v6 tcp dport 8658 accept
```

Validate and apply the persistent rules through the host's normal firewall configuration workflow. Keep the dedicated sets empty in the persistent configuration. A reload that recreates them starts empty, and the helper never reconstructs an old grant. Inspect the loaded rules with `sudo nft -a list chain inet public_edge_guard input`. CrowdSec, later `ip` and `ip6` input chains, upstream firewalls and the SSH authentication policy still apply. A later base chain can drop a packet after this chain accepts it.

Komari manages only these two sets. Direct UFW, firewalld and legacy iptables integration and source CIDR ranges are outside this implementation. A host can keep other firewall tooling if its normal policy allows the connection through and its reloads preserve the empty entry point.

## Request and confirm access

Open the node's Rescue panel and select Temporary SSH. The panel fills in the administrator's source IP as seen by the panel. Adjust it if SSH uses a different egress address, then complete two-factor verification. The port comes from the node. The helper first checks for a local SSH listener in the requested address family, then writes the source address and kernel timeout atomically. Two-factor approval binds the source, port, enrollment and any grant being replaced.

Behind NAT, enter the public egress address of the device that will run SSH. On that device, using the same network or VPN, run:

```bash
curl -4 -fsS https://api.ipify.org
```

Use `curl.exe` in Windows PowerShell. Use `ssh -4` for the matching IPv4 connection. If SSH takes a different route, for example through a jump host or a split-tunnel VPN, use the source address the node sees for that route. In an existing SSH session on the same path, the first field of `printf '%s\n' "$SSH_CONNECTION"` is that address. Request a fresh window if your egress address changes.

The receipt reports the listener check and the last confirmed kernel write and expiry. Check connectivity from the authorized machine using its usual SSH credentials. Test a fresh connection after revocation or timeout. Existing SSH connections can continue if the host's established-connection rules admit them.

An operator can revoke the exact grant early. The panel shows a pending revocation until it has a receipt or the window expires. A brief disconnect pauses new rescue work and preserves the original deadline. The helper closes the grant on a verified enrollment withdrawal delivered over its live watch or on confirmed invalidation of local credentials. If a disconnected node cannot receive a withdrawal and still has local credentials, the kernel deadline ends admission. Reconnects and duplicate requests never extend a window. A firewall reload or host reboot never restores an old grant.

For the first host acceptance, verify a successful SSH handshake from the permitted address, unsuccessful new connections from a second address, explicit revocation, one full 15-minute window and a firewall reload. `sudo journalctl -u komari-agent-rescue.service --since '15 minutes ago'` shows the grant ID, interface, port, source and expiry for confirmed writes and revocations. The administrative panel does not initiate an external probe.

## Troubleshoot by error code

The Rescue Helper output shows the code and local error details. Fix the cause, then reopen the node's Rescue panel after its next status report. Configuration and rules are checked again on each report.

### SSH_CONFIG_MISSING

The helper could not find the configuration file on this node. Run the automatic setup in [Configure the node](#configure-the-node) on the destination SSH node.

### SSH_CONFIG_INVALID

The helper could not read or validate the JSON. Check the reported path, root ownership, mode `0600`, and that the file and parent directories are trusted and contain no symlinks. Verify the four keys and the configured TCP port.

### SSH_NFT_READ_FAILED

The helper could not read or decode the configured nftables table. Run `sudo nft list tables` on the destination node. Check the configured table name and use `sudo nft -j list table inet <table>` to check whether nftables can return it. Use this node's table name, which may differ from the example. Check the helper journal if the command succeeds manually.

### SSH_NFT_RULES_INVALID

The table exists, but the expected input chain, two timeout sets, or matching rules could not be verified. Compare `sudo nft -a list chain inet <table> <chain>` and the two sets against [Configure the node](#configure-the-node). Keep the exact match order, interface and port. Reference each set only from its designated rule and put both rules before the public interface's drop rule. Apply changes through the host's persistent firewall configuration.

### SSH_STATE_INVALID

The helper could not read or reconcile its local grant state with the kernel sets. Read the local error details and inspect the two sets and the helper journal. Keep both sets empty in the persistent firewall definition. A reload that recreates them ends an active window. If the error refers to a damaged `*.temporary-ssh-state.json` file, retain it for diagnosis and review its path and ownership before requesting a new window.

### SSH_HELPER_UPDATE_REQUIRED

The running helper did not report support for source-scoped temporary SSH. Update the Agent and installed Rescue Helper together on the selected node. Check `sudo systemctl status komari-agent-rescue.service` and reopen Rescue after a fresh report.

### SSH_STATUS_UNAVAILABLE

The helper reported support without a usable port or a diagnostic code. Update the helper, then review its next status report. If this persists, confirm the JSON file and loaded firewall entry point on the same node.

### The kernel write succeeded, but SSH still times out

Confirm the reported port is listening, for example with `sudo ss -ltn 'sport = :8658'`. Verify the source address and IP family, then check rule placement, later input chains, CrowdSec and any cloud firewall. Test a fresh SSH connection from the authorized device.
