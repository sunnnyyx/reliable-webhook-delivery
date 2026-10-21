# Free public demo: GitHub sign-in

Use Render's free web service, Neon PostgreSQL, and Supabase GitHub authentication. Users click **Continue with GitHub** and receive their own workspace after verified sign-in. No email/password form, verification email, SMTP service, App Password, or purchased domain is needed. Users need a GitHub account.

## 1. Configure GitHub and Supabase

1. In GitHub, open **Settings → Developer settings → OAuth Apps → New OAuth App**. This is an OAuth App, not a GitHub App or a personal access token.
2. Set the application name to **RelayForge Delivery Console**.
3. Use your deployed Render URL as the Homepage URL. For local testing, use `http://127.0.0.1:8091` for now; update it after deployment.
4. Set **Authorization callback URL** to the callback shown in Supabase → Authentication → Sign In / Providers → GitHub. It has this form:

   ```text
   https://YOUR_PROJECT.supabase.co/auth/v1/callback
   ```

   This is Supabase's callback, not the app's own callback.
5. Register the app, copy its **Client ID**, and generate a **Client Secret** yourself. Enter both directly in the Supabase GitHub provider settings and save/enable that provider. Keep the Client Secret out of chat, Git, and the application environment. Supabase handles the GitHub credential exchange.
6. In Supabase → Authentication → URL Configuration, set the Site URL to your deployed origin and allow these exact app callback URLs:

   ```text
   https://YOUR_SERVICE.onrender.com/auth/callback
   http://127.0.0.1:8091/auth/callback
   ```

   Add only the local URL while testing if the Render URL is not known yet. Avoid wildcard redirect destinations.
7. Allow new users to sign up. Disable the Email provider if GitHub should be the only provider; keep other security protections enabled.

GitHub consent is for identity/email access, not repository access. The application does not request repository scopes. Review the OAuth consent screen before authorizing. See [Supabase GitHub setup](https://supabase.com/docs/guides/auth/social-login/auth-github).

## 2. Test GitHub sign-in locally

Your existing private `.env` needs:

```env
PROFILE=development
CONSOLE_AUTH=github
SUPABASE_URL=https://YOUR_PROJECT.supabase.co
SUPABASE_PUBLISHABLE_KEY=sb_publishable_YOUR_KEY
```

Preserve the existing database credentials and encryption key. Then, from the project root:

```sh
make dev-up
make migrate-up
make build
```

In separate terminals:

```sh
make local-api
```

```sh
make local-worker
```

```sh
make local-console
```

Stop any older console process in its owning terminal before restarting it. Open `http://127.0.0.1:8091`, click **Continue with GitHub**, authorize the OAuth app, and return to the dashboard. This creates a new workspace for that verified Supabase identity; it does not attach a public user to the old CLI-created Demo tenant.

Set `CONSOLE_AUTH=api-key` to retain the original local API-key login. Hosted production mode always requires GitHub identity.

## 3. Create the hosted database

Create a Neon free project and copy its direct PostgreSQL connection URL (or a supported session pooler). Use `sslmode=verify-full`; retain other required connection parameters. The Docker image includes system CA certificates. Never disable certificate verification to bypass a connection error.

Use a separate database from the local demo. Application events, tenant mappings, and API keys live in PostgreSQL. The container filesystem is ephemeral. Startup applies embedded migrations; use a single instance and review/back up before future schema changes.

## 4. Deploy to Render

Push the source, then create a Render Blueprint from root `render.yaml`, or create a Docker web service with root `Dockerfile` and choose **Free**.

Set the following in Render's environment settings:

| Setting | Value |
| --- | --- |
| `PROFILE` | `production` |
| `CONSOLE_AUTH` | `github` |
| `PUBLIC_URL` | Exact service origin, e.g. `https://YOUR_SERVICE.onrender.com`, without trailing slash |
| `DATABASE_URL` | Neon URL with `sslmode=verify-full` |
| `ENCRYPTION_KEY` | Base64 of 32 random bytes; preserve across redeploys |
| `SUPABASE_URL` | Your Supabase project HTTPS origin |
| `SUPABASE_PUBLISHABLE_KEY` | Your `sb_publishable_...` key; not a secret/service-role key |

Generate a new master key for a new hosted database:

```sh
python3 -c 'import secrets,base64; print(base64.b64encode(secrets.token_bytes(32)).decode())'
```

Keep the key in your password manager. If migrating existing encrypted data, use the existing key instead. Do not copy the entire local `.env`; development receiver exceptions and local credentials do not belong in public deployment.

Render supplies `PORT`. The Blueprint supplies conservative pool/concurrency/backlog settings, disables optional Redis admission, and checks `/health/ready`. The non-root `web` process supervises private API, console, and worker processes behind one web port. Only `/v1/*` routes to the API; operational metrics stay on loopback. Receivers must be public HTTPS URLs in production.

After Render assigns the final URL, confirm `PUBLIC_URL`, the GitHub Homepage URL, Supabase Site URL, and the app callback allowlist match it, then redeploy if needed.

## Free plan limits

Render sleeps free services after 15 minutes without inbound traffic. The worker shares that service, so **webhook processing pauses while it sleeps**. Dashboard/API traffic wakes it. PostgreSQL retains work, but the existing retry and 24-hour cycle-age limits still apply; old work may need explicit replay. Cold starts delay first requests.

This is suitable for a small public demo, not an always-on delivery guarantee. Render can also suspend free services with unusually high outbound traffic, including database calls. Monitor usage and avoid enabling paid billing inadvertently. Use always-on compute for continuous delivery.

Provider limits and policies can change. See [Render free services](https://render.com/docs/free), [Neon pricing](https://neon.com/pricing), and [Supabase pricing](https://supabase.com/pricing).

## Verification before sharing

Use two different GitHub accounts in separate browser profiles. Confirm they receive separate workspaces and cannot see each other's endpoints, events, or integration keys. Create an endpoint and integration key, publish to the hosted `/v1/events` API, inspect acknowledgment, then revoke the key and confirm 401. Check sign-out, service restart, and a real sleep/wake cycle. Restart retains workspace data but ends browser sessions.

Automated tests cover PKCE challenge generation, browser-bound/expiring/single-use callbacks, authoritative identity checks, cross-origin rejection, secure session cookies, tenant isolation, and API-key expiry. Live GitHub consent, provider configuration, database TLS and Render behavior require your real provider projects and are not established by fixtures.

## Security boundaries

The server stores PKCE verifiers for at most five minutes and binds each callback to an HttpOnly SameSite=Lax flow cookie. Supabase authorization codes are exchanged server-side, followed by a remote `/user` check requiring a GitHub identity. The browser receives only a Secure, HttpOnly, SameSite=Strict application session cookie; provider/access/refresh tokens never enter browser JavaScript or localStorage. The callback is the only cross-site GET exception. Other browser operations keep exact Host/Origin and CSRF protection.

Sessions last 30 minutes, are bounded to 128 per process, and end on restart. Browser API keys independently expire in PostgreSQL. Integration keys remain active until revoked. Tenant ownership uses the verified immutable Supabase user ID, not a browser-supplied tenant or editable profile. Horizontally scaled deployments need shared sessions or sticky routing; this Blueprint uses one instance.

Backups/restore drills, retention cleanup, monitoring, abuse controls, secret rotation, egress policy, and capacity planning remain operator responsibilities. Account deletion or bans at an external identity provider do not instantly cancel an already issued 30-minute local session; use the private admin CLI to disable its tenant for immediate API denial.
