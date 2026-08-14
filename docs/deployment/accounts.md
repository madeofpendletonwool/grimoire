![Accounts](../assets/sprites/key.png){: style="float:right; margin-left:1rem" align=right }

# Accounts & Invites

Grimoire is single-household software, so it has a login but no open sign-up.

## The first keeper

On a fresh install the app has no accounts, and the login page offers to create the first one: whoever reaches the app claims it, and that first account is the **admin** — the keeper. Account creation then closes, and later arrivals only see a login form.

There is no seeded password to forget to change; the first visit *is* the provisioning.

## Invites

The admin invites friends in from **Settings → Invitations**: each click mints a single-use invite link (`/?invite=…`) that lets exactly one person make an account, then spends. The admin can see which links are pending, used, or expired, and revoke a pending one.

!!! note "Shown once, at creation"

    Invite codes are treated the same way as session tokens: the link carries the raw secret, the database stores only its SHA-256 digest, and the raw code is returned to the admin **once**, at creation — the list never shows it again.

Self-service creation stays off by default — an invite is the only way in past the first keeper. (`GRIMOIRE_OPEN_REGISTRATION=true` reopens it, the original escape hatch; invites are the recommended path.)

## The security model

- **Passwords** are hashed with **argon2id** (64 MiB, 3 passes) using a per-password random salt; the parameters travel inside every stored hash, so raising the cost later leaves existing passwords verifiable.
- **Sessions are server-side**: the cookie holds an opaque 256-bit token, the database holds only its SHA-256 digest, and signing out deletes the row — a stolen cookie stops working the moment you log out. The cookie is `HttpOnly` and `SameSite=Lax`, and is marked `Secure` automatically when the request arrives over TLS or through a proxy that sets `X-Forwarded-Proto: https`.
- **Scoped per account**: conversations and study progress belong to their account.
- **Accounts live in the same SQLite file** as everything else, so they survive `grimoire index` and ride along with the `/data` volume.

## Upgrading

- **From a version without accounts?** Conversations recorded under the old anonymous owner are handed to the first account created, so no history is lost behind the new login.
- **From a version before admins/invites?** The oldest existing account is marked admin on the way up, matching "first created user is the admin."

## Tunables

| Variable                     | Default | Notes                                                                     |
| ---------------------------- | ------- | ------------------------------------------------------------------------- |
| `GRIMOIRE_SESSION_TTL`       | `720h`  | How long a session lasts. Any Go duration (`24h`, `168h`, …).              |
| `GRIMOIRE_INVITE_TTL`        | `168h`  | How long a freshly minted invite link stays usable. `0` = never expire.   |
| `GRIMOIRE_OPEN_REGISTRATION` | `false` | Leave self-service account creation open after the first keeper.          |
