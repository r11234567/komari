# Lite billing follow-up

Behavior and regression fixtures ported from MIT-licensed nuomiiiii/Lite (a73311d35f06d5f8f7a33937856dc485b88bfc80; scope unchanged through 3ad4d56), notably a4b8cd9, 5eba973, 2d05866, 643f409, 3d26308 and f8517fd. Upstream copyright/license remains in the repository license notices; this port preserves the existing Komari architecture.

Adds FX backfill/upgrade locking, direct original-currency conversion, HKD, zero fee entries, live node names/region overrides, reset-clock committed cycles and ceil remaining days for finite free/one-time nodes. Keeps the fork's accrual cache and invalidation.

Expiry is stored as an absolute UTC instant with an IANA timezone. The authenticated billing-policy endpoint gives the editor the authoritative wall clock; unchanged edits preserve the precise instant. Renewal waits for the actual instant, maintains the local clock through DST, and compares the old expiry before updating. Missing wall times are rejected for manual input; recurring spring gaps advance by the gap and overlaps choose the earlier occurrence, matching upstream.

Validation runs in GitHub Actions, including raw failure output and race checks; artifacts are retained 30 days. No local builds or E2E.
