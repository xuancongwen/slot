# Slot

A small Google Calendar booking app for a home lab. One Go process, one SQLite database, two HTTP listeners. Server-rendered HTML, local CSS, and about 2 KB of optional JavaScript. No Node runtime, Redis, mail server, or external frontend assets.

> **Status: not yet functional.** Slot is a work in progress and is not ready for use. The features below describe the intended first version, not a working release.

## First version

- Multiple independent scheduling users, registered through the admin port.
- Multiple Google accounts per user, with multiple calendars per account.
- Combined free/busy checks across selected calendars; one destination calendar for bookings.
- A public booking link for each user: `/b/your-name`, listing their meeting types at `/b/your-name/type`. No public user directory.
- Multiple meeting types per user, each with its own length, IANA timezone, weekly availability, buffers, minimum notice, and booking horizon. A per-type timezone lets you publish a schedule for a trip alongside your usual one.
- Full days off per user.
- Locations in each user's profile: Google Meet (a fresh link per booking, created by Google) plus any links or addresses, such as a Zoom room. Guests pick one, the default preselected, or enter their own.
- Pause/publish controls, upcoming and past booking list, cancellation, and password changes.
- Google invitations and an optional `.ics` download.
- Durable Google write retries, including after a restart. Uncertain bookings continue reserving the slot.
- Separate public/admin route tables. Registration and account operations do not exist on the public listener.

The app starts without Google credentials so you can create an account and explore the admin UI. To publish a working booking page, connect Google and choose a destination calendar.

## Run locally

Requires Go 1.26 or newer. Go 1.27 is used in the container build.

```sh
make run
```

`make build` builds `bin/slot` without running it.

- Admin: <http://localhost:8081>
- Public: <http://localhost:8080>
- Data: `./data/slot.db` and `./data/secret.key`

Create your host account on the admin port. Configure Google below, restart, connect an account, save calendar selections, edit your meeting type's timezone and hours, then publish the page.

## Run with Docker Compose

```sh
cp .env.example .env
# Edit .env with your origins and Google OAuth credentials.
make up
```

The default published ports bind to host loopback. A reverse proxy running on the Docker host can forward to them. For a proxy on another machine, change the host bindings to a specific LAN address and restrict access with your firewall. For a proxy in another container, attach it to the same Docker network and route to `slot:8080` and `slot:8081`.

The container runs as a non-root user, with a read-only root filesystem and a writable named volume for `/data`. There is no shell in the runtime image. To move architectures, build the image on that host, or use Docker Buildx for `linux/amd64` or `linux/arm64`.

## Deploy to a Debian 13 LXC

`deploy/setup.sh` prepares a bare Debian 13 container, and `deploy/deploy.sh` builds and ships Slot to it. Both work as `root`; no users are added. Slot runs as root under a systemd sandbox that leaves only `/var/lib/slot` writable and drops every capability.

Container to create (Slot uses about 25 MB of memory; the rest is Debian and headroom):

| Setting | Value |
| --- | --- |
| Template | Debian 13 (trixie), unprivileged |
| Features | `nesting=1`, needed by the systemd sandbox |
| CPU | 1 core. Each sign-in costs one PBKDF2 hash (about 0.1–0.3 s of a core); 2 cores if several hosts sign in at once. |
| Memory | 256 MB, plus 256 MB swap |
| Disk | 4 GB |
| Network | A fixed address (static or DHCP reservation) that your reverse proxy can reach on 8080; keep 8081 to your LAN or VPN |

Then, from your checkout:

```sh
scp deploy/setup.sh root@slot.lan: && ssh root@slot.lan ./setup.sh
ssh root@slot.lan editor /etc/slot/slot.env   # origins and Google OAuth client
deploy/deploy.sh root@slot.lan
```

`deploy.sh` runs `make check`, cross-compiles for the container's architecture (amd64 or arm64), backs up the database to `/var/backups/slot` (the last 10 are kept), swaps the binary, and waits for `/healthz`. If the new build does not come up, it restores the previous binary. Configuration lives in `/etc/slot/slot.env`; after editing it, run `systemctl restart slot`. Logs: `journalctl -u slot`.

## Google setup

1. Create or choose a Google Cloud project and enable **Google Calendar API**.
2. Configure the OAuth consent screen. For a private trial, use External / Testing and add each Google account you want to connect as a test user.
3. Create an OAuth client of type **Web application**.
4. Add an authorized redirect URI matching your admin origin exactly:
   - Local: `http://localhost:8081/oauth/callback`
   - Deployed example: `https://slot-admin.example.com/oauth/callback`
5. Set `GOOGLE_CLIENT_ID` and `GOOGLE_CLIENT_SECRET`. These are server configuration values, not the host’s Google password. Restart the app.
6. Sign in on the admin port and select **Connect or reconnect Google**. Approve the requested Calendar permissions. Repeat for additional Google accounts.
7. Choose calendars under **Check for conflicts**, choose a writable destination, and save.

The redirect returns through the user's browser. Google does not need to reach your private admin server directly, but the browser must be able to reach it. Google's redirect URI rules still apply; localhost HTTP is permitted for local development, while a deployed setup should use a domain and HTTPS rather than a private IP redirect.

The app requests calendar-list read access, free/busy access, and event read/write access. It uses the OAuth library's PKCE support, a short-lived one-time state bound to the signed-in session, offline access, and encrypted refresh-token storage. Each Google connection belongs to the host who authorized it.

Google OAuth apps in External / Testing can issue refresh tokens that expire after seven days for these scopes. Reconnect when necessary; for longer-lived use, configure the appropriate production consent status. Google may require verification depending on audience and use. Google Workspace administrators may also need to permit the client.

References: [Google web-server OAuth](https://developers.google.com/identity/protocols/oauth2/web-server), [free/busy API](https://developers.google.com/workspace/calendar/api/v3/reference/freebusy/query), [event insertion and invitations](https://developers.google.com/workspace/calendar/api/v3/reference/events/insert).

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `PUBLIC_ADDR` | `:8080` | Public listener bind address; Docker sets `:8080`. |
| `ADMIN_ADDR` | `127.0.0.1:8081` | Admin listener bind address; Docker sets `:8081` inside the container. |
| `PUBLIC_URL` | `http://localhost:8080` | Canonical public origin; used in booking links and Google invitations. |
| `ADMIN_URL` | `http://localhost:8081` | Canonical admin origin; used for OAuth and form origin checks. |
| `DATA_DIR` | `data` | Persistent database/key directory; Docker uses `/data`. |
| `GOOGLE_CLIENT_ID` | unset | Google OAuth Web application client ID. |
| `GOOGLE_CLIENT_SECRET` | unset | Google OAuth client secret. |
| `REGISTRATION_OPEN` | `true` | Set to `false` to disable host registration. Existing users can still sign in. |
| `REGISTRATION_CODE` | unset | Optional shared code required to register a host. |
| `PUBLIC_PORT`, `ADMIN_PORT` | `8080`, `8081` | Compose host-port mappings only. Update origins and Google redirects if changed. |

The standalone binary does **not** read `.env`. Export variables in the shell or use your service manager's environment file. Compose reads `.env` for the interpolation shown in `compose.yaml`.

Public and admin origins must differ and cannot include URL paths. Use a dedicated public domain and private admin domain with HTTPS in deployment. Keep the admin listener on your LAN/VPN or localhost. A different port is routing separation, not a network firewall. Registration is intended for trusted host users with access to the admin interface; accounts have no privileged cross-user administrator role.

The app does not trust `X-Forwarded-For`. Its in-process rate limit therefore groups clients behind a reverse proxy under that proxy's IP. For higher public traffic, enforce per-client limits at the proxy and adjust the application limiter deliberately.

## Booking behavior and reliability

1. Slot generation follows the meeting type's timezone and weekly hours. Google is queried for current busy intervals across selected calendars, in batches of at most 50 per account.
2. Availability is checked again on submission. Errors or missing Google calendar results close availability rather than treating the calendar as empty.
3. An SQLite trigger reserves the interval atomically. Pending and cancelling bookings block time too. Hosts using the same Google destination calendar cannot reserve overlapping times through Slot.
4. A background worker runs every 15 seconds and inserts the Google event using a stable ID. A timed-out write can be retried without intentionally creating a second event. The signed form ticket makes duplicate form submissions return the same booking.
5. The booking is marked confirmed only after Google accepts it or an earlier insertion is verified. Google is asked to notify the guest. The confirmation page accurately describes pending states and refreshes while waiting.
6. Cancellation is also durable: the slot is not released until Google confirms deletion or reports that the event is already absent.

Google failures back off up to one hour. The admin booking list shows sync errors and offers **Retry sync** and **Cancel**. A pending reservation does not silently expire: a timeout may mean Google already created the event. Reconnect the account if its authorization has expired.

The buffer is applied on **each side** of a Slot booking. Two Slot meetings with 10-minute buffers require 20 minutes between their actual meeting times. Existing non-Slot Google events require the new booking's 10-minute separation.

Google Calendar does not offer an atomic “insert only if still free” operation. A manual Google event or another scheduling app can race the final free/busy check. Slot protects its own reservations; it cannot lock outside calendar writers. A selected calendar shared across different hosts can also change externally between reads.

## Intentional first-pass limits

- One daily working-hours interval per meeting type, and full-day exceptions per host.
- Guests pick a date on a month calendar and see times in their own timezone, detected by the browser and changeable on the page. Without JavaScript the page shows the meeting type's timezone. Calendar invitations display in the guest's own calendar timezone.
- Reschedule by cancelling and booking again. No payments, round-robin teams, reminder service, or per-booking Zoom meetings; a Zoom location is a fixed room link.
- No account email verification, forgotten-password email flow, or site-wide super-admin. Protect registration through the private admin interface and optional code.
- Reconnect/refresh Google accounts from the admin page. This version does not include account deletion or a disconnect UI; Google permissions can be revoked from the Google account, which causes affected booking operations to fail closed until reconnected or reconfigured.
- Google free/busy is live, but external event edits/deletions do not rewrite Slot's booking records. Manage Slot bookings in Slot. If someone deletes a Slot event directly in Google, cancel its record in Slot to release the local reservation.
- Invitations are delivered by Google, subject to its policies, quotas, and the guest's invitation settings. Slot does not run SMTP or guarantee email delivery.
- Run **one app instance per database**. SQLite and the worker are designed for a small single-server deployment; do not run replicas against a shared network-mounted database.

## Data and backups

Databases from schema version 4 onward are upgraded in place at startup. Older development databases cannot be; if Slot refuses to start with a schema version error, move the data directory aside and start fresh.

The data directory contains the SQLite database, WAL/SHM files while running, and `secret.key`. The key encrypts Google tokens and signs booking forms. **Back up the key with the database** and keep it private. Losing it makes saved Google credentials unusable. The app refuses to create a replacement key next to an existing database.

For the simplest consistent backup, stop Slot, copy/archive the entire data directory or named volume, then restart. To restore, stop the app and restore that complete backup with permissions suitable for the service user (container UID 65532). For online backups, use SQLite's backup API or a WAL-aware backup tool; copying only a live `.db` file is insufficient.

Booking names/emails and host profile data are stored in SQLite as plaintext; only Google tokens are encrypted. Secure the data directory and backups accordingly. No analytics or third-party frontend requests are included.

## Development and verification

```sh
make check   # gofmt check, go vet, and race-enabled tests
make bench   # slot-generation benchmark
make fmt     # format the code
```

Tests cover concurrent reservations, shared destination conflicts, DST gaps/repeated hours, host isolation, public/admin route separation, CSRF rejection, signed ticket integrity, registration/login, restart recovery, encrypted token refresh, Google API batching/errors, idempotent insertion, cancellation retries, and the end-to-end HTTP booking flow with a fake Calendar service.

The real Google consent/invitation flow requires your OAuth client and Google accounts. Automated tests use local HTTP doubles; they do not connect to anyone's calendar.

Layout:

- `cmd/slot/`: the entry point; reads configuration and runs the server.
- `internal/slot/`: the application, one file per concern: `app.go` (startup, keys, `Run`), `config.go`, `schema.go` and `schema.sql` (storage and upgrades), `store.go` (records and queries), `availability.go` (slot generation), `google.go` and `oauth.go` (Calendar API and connecting accounts), `worker.go` (durable writes), `middleware.go` and `auth.go` (headers, CSRF, rate limits, sessions), and the HTTP handlers in `routes.go`, `dashboard.go`, `meeting_types.go`, `locations.go`, `booking.go`, and `manage.go`. Tests sit beside the file they cover.
- `internal/slot/web/`: embedded templates and static assets.
