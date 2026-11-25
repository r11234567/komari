# Serving custom themes on a separate origin

The shared listener serves the embedded public and management applications.
Installed themes, custom head and body HTML, and public plugin HTTP routes and
hooks need a dedicated public hostname. Their stored settings stay in the
database while the shared listener is in use.

Configure both upstreams in the reverse proxy. For example:

| External origin | Komari listener | Environment |
| --- | --- | --- |
| `https://status.example.com` | `127.0.0.1:25774` | `KOMARI_LISTEN`, `KOMARI_PUBLIC_ORIGIN` |
| `https://panel.example.com` | `127.0.0.1:25775` | `KOMARI_PANEL_LISTEN`, `KOMARI_PANEL_ORIGIN` |

The proxy forwards the external hostname and HTTPS scheme using the configured
trusted proxy settings. Point Agents and management browsers to the panel
hostname. Keep the Cloudflare Agent gRPC setting when proxying Agent traffic
through the Cloudflare orange cloud.

When a hostname previously served third-party scripts, use a fresh hostname
for the panel. Browsers may still have a service worker or cache from the old
origin. Sign in on the new hostname and register a Passkey for the new RP ID
using a retained administrator sign-in method before retiring the old entry.

On the deployed hosts, verify that `/` on the public hostname renders the
selected theme, `/admin` returns 404 there, and the panel hostname serves
`/admin` while returning 404 for `/themes/` and public plugin URLs.
