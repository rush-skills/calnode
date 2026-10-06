# Deploying Calnode to Fly.io from the command line

`calnode-fly.sh` does every infrastructure operation for one Calnode instance on Fly.io,
driven by a `.env` file, so nobody has to click through dashboards or run `fly` commands by
hand. One instance = one `.env`. Keep `.env` out of git (it is ignored) and back it up: it
holds the encryption keys.

## Setup

```bash
# 1. tools
brew install flyctl            # or https://fly.io/docs/flyctl/install/
fly auth login                 # or put a token in FLY_API_TOKEN

# 2. config
cp deploy/fly/.env.example deploy/fly/.env
$EDITOR deploy/fly/.env        # app name, org, region, BASE_URL at least

# 3. first deploy: creates the app and volume, generates the encryption keys into .env,
#    sets the secrets, builds this checkout and deploys it
deploy/fly/calnode-fly.sh init
```

Open `BASE_URL` and create the owner account. Then create an API key (Admin → API Keys),
put it in `CALNODE_API_KEY`, fill the `SETTINGS_*` lines and run
`deploy/fly/calnode-fly.sh settings apply` to configure the workspace without the UI.

Set `EMBED_ALLOWED_ORIGINS` to the sites that embed the booking widget or the lead-form
picker, and leave `TRUSTED_PROXY_CIDRS=172.16.0.0/12` so rate limits key on the visitor
rather than on Fly's proxy.

## Everyday commands

| Command | What it does |
|---|---|
| `deploy` | Build the current git checkout with Fly's remote builder and deploy it. Prints the deployed version and health. |
| `secrets` | Push every non-empty variable from `.env` as a Fly secret (restarts the app). |
| `status` / `logs [--tail]` | Machine state, deployed commit, health; recent or live logs. |
| `env check` | Validate `.env` and show the secrets and config that would be applied, without changing anything. |
| `scale --memory 2gb --min 1` | Change VM memory / always-on machines (updates `.env`, redeploys). |
| `settings show` / `settings apply` | Read or push workspace settings through the admin API: default participants, invite message, host-name switch, business name, sign-in domains. |

## Custom domain (Cloudflare)

```bash
deploy/fly/calnode-fly.sh domain add bookings.example.com   # requests the certificate, prints DNS records
# add the CNAME at Cloudflare as "DNS only" (grey cloud)
deploy/fly/calnode-fly.sh domain check bookings.example.com # until it says Issued
deploy/fly/calnode-fly.sh domain set bookings.example.com   # BASE_URL → https://…, secrets pushed, app restarted
```

Google OAuth redirect URIs follow `BASE_URL`: `/v1/auth/callback` and `/v1/calendar/callback`.

## Backups

Backups are off until `LITESTREAM_REPLICA_URL` (plus endpoint, region and keys for an
S3-compatible bucket such as Cloudflare R2) is set in `.env`.

```bash
deploy/fly/calnode-fly.sh backup enable     # pushes the LITESTREAM_* secrets; replication starts on boot
deploy/fly/calnode-fly.sh backup verify     # restore drill inside the machine, touches only /tmp
deploy/fly/calnode-fly.sh backup download   # a consistent copy of the database on your machine
```

Run `backup verify` once after enabling and periodically after. With backups on, a lost or
replaced volume restores itself on the next boot.

## Recovery and teardown

| Command | What it does |
|---|---|
| `restore FILE.db --yes` | Stop the app, replace the database with `FILE.db`, start it. |
| `reset --yes` | Delete the database. The next boot shows first-run setup. Secrets stay. |
| `teardown --yes` | Destroy the app and its volume. Irreversible. `backup download` first. |

## Using it from CI

Everything reads environment variables, so a pipeline can export `FLY_API_TOKEN`, `FLY_APP`,
`BASE_URL`, `CALNODE_ENCRYPTION_KEY`, `CALNODE_RECOVERY_SECRET` (and the rest) as CI secrets and
run `deploy/fly/calnode-fly.sh deploy`. `CALNODE_ENV_FILE=/path/to/other.env` points the script
at a different file, for example one per environment.
