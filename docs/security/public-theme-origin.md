# Serving installed public themes

## One hostname

Set `KOMARI_ALLOW_SHARED_ORIGIN_THEMES=true` in the Komari process environment.
In Docker Compose, add it under the Komari service and recreate that service:

```yaml
environment:
  KOMARI_ALLOW_SHARED_ORIGIN_THEMES: "true"
```

Open **Settings → Theme** or **Theme Market**, choose an installed theme and
select **Set as Active Theme**. You can switch back to the built-in theme from
the same page. When the setting is unset, Komari serves the built-in theme on
the shared listener.

An activated theme runs on the same browser origin as the management panel.
Only activate themes you trust with access to that origin. Keep using separate
origins for untrusted themes. Site-wide custom HTML and public plugin HTTP hooks
are available through the dedicated public origin.

## Separate public and management hostnames

The dedicated public listener serves installed themes, custom head and body
HTML, and public plugin HTTP routes and hooks.

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
