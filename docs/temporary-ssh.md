# Temporary inbound SSH on a Linux node

Temporary SSH admits one source IPv4 or IPv6 address to a fixed SSH port for 15 minutes. The node needs an updated Agent and authorized rescue helper. The host administrator prepares a dedicated nftables entry point. Browser WebSSH remains a separate option.

## Configure the node

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

Open the node's Rescue panel, select Temporary SSH, enter the single IPv4 or IPv6 address as seen by the node, and complete two-factor verification. The port comes from the node. The helper first checks for a local SSH listener in the requested address family, then writes the source address and kernel timeout atomically. Two-factor approval binds the source, port, enrollment and any grant being replaced.

The receipt reports the listener check and the last confirmed kernel write and expiry. Check connectivity from the authorized machine using its usual SSH credentials. Test a fresh connection after revocation or timeout. Existing SSH connections can continue if the host's established-connection rules admit them.

An operator can revoke the exact grant early. The panel shows a pending revocation until it has a receipt or the window expires. A brief disconnect pauses new rescue work and preserves the original deadline. The helper closes the grant on a verified enrollment withdrawal delivered over its live watch or on confirmed invalidation of local credentials. If a disconnected node cannot receive a withdrawal and still has local credentials, the kernel deadline ends admission. Reconnects and duplicate requests never extend a window. A firewall reload or host reboot never restores an old grant.

For the first host acceptance, verify a successful SSH handshake from the permitted address, unsuccessful new connections from a second address, explicit revocation, one full 15-minute window and a firewall reload. `sudo journalctl -u komari-agent-rescue.service --since '15 minutes ago'` shows the grant ID, interface, port, source and expiry for confirmed writes and revocations. The administrative panel does not initiate an external probe.
