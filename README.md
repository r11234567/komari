> **Fork Notice**
>
> This repository is a fork of [komari-monitor/komari](https://github.com/komari-monitor/komari), maintained primarily
> for personal use, deployment, testing, and customization.
>
> It is **not an official upstream repository**, and changes made here
> should not be considered part of the upstream project.
>
> Please refer to the upstream repository for the official project,
> documentation, releases, and support.
>
> Upstream copyright, licensing, and attribution remain unchanged.

# Komari

![komari](https://socialify.git.ci/komari-monitor/komari/image?description=1&font=Inter&forks=1&issues=1&language=1&logo=https%3A%2F%2Fraw.githubusercontent.com%2Fkomari-monitor%2Fkomari-web%2Fd54ce1288df41ead08aa19f8700186e68028a889%2Fpublic%2Ffavicon.png&name=1&owner=1&pattern=Plus&pulls=1&stargazers=1&theme=Auto)

[English](./README.md) | [简体中文](./README_zh-cn.md)

Komari is a lightweight, self-hosted server monitoring solution. It provides a simple and efficient way to track server performance through a web interface, with metrics collected by a lightweight agent.


[Fork releases](https://github.com/r11234567/komari/releases/latest) · [Agent releases](https://github.com/r11234567/komari-agent/releases/latest) · [Upstream documentation](https://www.komari.wiki/)

## Features

- **Real-time monitoring**: Displays monitoring data at one-second intervals.
- **Lightweight and efficient**: Uses minimal system resources and works well on servers of any size.
- **Self-hosted**: Keeps you in control of your data and privacy.
- **Web interface**: Provides an intuitive, easy-to-use monitoring dashboard.
- **Extensible**: Supports custom themes and plugins.

## Deployment and upgrades

1. Back up the panel and data, then upgrade the panel before the Agents. Docker deployments can use `ghcr.io/r11234567/komari:<version>`. See [backup and restore](docs/backup-restore.md) and [Linux systemd updates and rollback](docs/self-update.md).
2. Configure the panel's HTTPS origin and trusted proxies. If you use the legacy Passkey plugin, follow the [native Passkey migration](docs/native-passkeys.md) and verify a backup sign-in method before upgrading. Sign in again after the session-cookie migration.
3. Follow the [Agent installation and verification guide](https://github.com/r11234567/komari-agent/blob/main/docs/service-startup.md). New nodes use browser approval. Preserve existing enrolled credentials when upgrading an Agent in place.

## Configuration guides

- [Cloudflare DNS proxy](docs/cloudflare-agent-grpc.md) for Agent WebSSH and file-operation streams over gRPC.
- [Traffic accounting](docs/traffic-accounting.md) for panel-managed billing periods, calibration and delayed Agent samples.
- [Temporary SSH access](docs/temporary-ssh.md) for nodes with the optional rescue helper and a configured nftables entry point.
- [Historical CSV worker](docs/background-csv-worker.md) for exporting retained raw metric points with a separate Linux worker.
- [Public theme and admin origin isolation](docs/security/public-theme-origin.md) for sites using third-party themes or plugins.

## Running behind a reverse proxy or an IP banning layer

### Declare your trusted proxies

Set `KOMARI_TRUSTED_PROXIES` to say which peers are allowed to declare the real
client address through `X-Forwarded-For` / `X-Real-Ip`:

| Value | Meaning |
| --- | --- |
| unset | Trust loopback proxies (`127.0.0.1,::1`) only. |
| `127.0.0.1,::1` | A reverse proxy runs on the same host. Use this for the common setup. |
| `none` | Komari is exposed directly. Forwarding headers are ignored and the transport peer is used. |
| a comma-separated list | Trust exactly these hosts or CIDRs, e.g. `10.0.0.0/8,192.168.1.5`. |

For a proxy connecting from a container or a separate host, list its actual
source address or CIDR explicitly. Komari uses this policy for rate limiting,
audit logs and login sessions. Invalid or unrestricted values prevent startup.

### How an external banning layer should judge Komari traffic

Every response carries `X-Komari-Principal`, naming how the request
authenticated: `agent`, `user`, `api-key`, or `anonymous`. Log that header and
decide on it, rather than pattern-matching URLs:

- **Do not count** requests where the header is `agent`, `user` or `api-key`.
  These are authenticated Komari clients. Their addresses change - a home
  broadband agent's prefix is not stable - so an address-based allowlist cannot
  express this, while an allowlist of API paths would also excuse an attacker
  who guessed those paths.
- **Do count** requests where the header is `anonymous`, and requests with no
  such header at all. That covers scanners probing `/actuator`, `/.env` and
  friends, unauthenticated traffic against real API paths, and anything not
  served by Komari.
- `anonymous` deliberately does not distinguish "presented no credential" from
  "presented one that was rejected", because reporting that difference in a
  proxy log would tell an observer which tokens and accounts exist.

Two things worth knowing before you set thresholds:

- **HTTP 429 is a normal, self-correcting outcome, not evidence of an attack.**
  It is returned by the optional request rate limiting, and independently by
  expensive historical reads when they shed load - the latter regardless of
  whether the rate-limit setting is on. A browser opening several charts at
  once can legitimately see one. Every 429 carries an accurate `Retry-After`.
- **Do not add a concurrency cap on the metric-read endpoints.** Those reads
  already coalesce identical in-flight queries, cache briefly, and bound their
  own concurrency, so an external cap is a second, tighter limit on the same
  work and mostly punishes a dashboard loading its charts.

Note also that the panel accepts an agent token through a query parameter for
backwards compatibility, so exclude query strings from your access log format
if agents may use that form - otherwise tokens end up in the log the banning
layer reads.

## Screenshots

| Page                | Screenshot                                                                                                                                                             |
| ------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Home Dashboard      | <img src="https://b2.akz.moe/awesome-pictures/komari-screenshot/%E4%B8%BB%E9%A1%B5%E4%BB%AA%E8%A1%A8%E7%9B%98-en.webp" width="800" alt="Home Dashboard">               |
| Admin Dashboard     | <img src="https://b2.akz.moe/awesome-pictures/komari-screenshot/%E5%90%8E%E5%8F%B0%E4%BB%AA%E8%A1%A8%E7%9B%98-en.webp" width="800" alt="Admin Dashboard">              |
| History Charts      | <img src="https://b2.akz.moe/awesome-pictures/komari-screenshot/%E5%8E%86%E5%8F%B2%E5%9B%BE%E8%A1%A8-en.webp" width="800" alt="History Charts">                        |
| Web Terminal        | <img src="https://b2.akz.moe/awesome-pictures/komari-screenshot/%E7%BD%91%E9%A1%B5%E7%BB%88%E7%AB%AF.webp" width="800" alt="Web Terminal">                             |
| Customizable Themes | <img src="https://b2.akz.moe/awesome-pictures/komari-screenshot/%E4%B8%BB%E9%A2%98%E5%8F%AF%E8%87%AA%E5%AE%9A%E4%B9%89-en.webp" width="800" alt="Customizable Themes"> |
| Theme Market        | <img src="https://b2.akz.moe/awesome-pictures/komari-screenshot/%E4%B8%BB%E9%A2%98%E5%B8%82%E5%9C%BA-en.webp" width="800" alt="Theme Market">                          |


Before building from source, run `git submodule update --init --recursive`. See the [Proto dependency guide](docs/proto-dependency.md).
