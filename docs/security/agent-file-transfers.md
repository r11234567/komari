# Agent file transfers

Update the panel and Agents together for workspace uploads, downloads and file
previews. The Agent uses JWT DPoP for its enrolled requests. It may request a
nonce from `/ping` before sending a non-replayable stream. That response cannot
select the proof format.

The panel accepts the one-time transfer ticket in `X-Komari-Transfer-Token` and
verifies the Agent proof against the request method and transfer URL. The
Agent parent signs the request and the account worker accesses a one-use local
relay. Keep `Authorization`, `DPoP`, `DPoP-Nonce` and
`X-Komari-Transfer-Token` intact at the reverse proxy.
