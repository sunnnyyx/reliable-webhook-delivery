# No-login public demo deployment

The default Docker image runs `cmd/demo`: one process containing the console, isolated temporary workspaces, delivery workers, and a signed loopback HTTP receiver. No PostgreSQL, Supabase, GitHub OAuth app, SMTP, purchased domain, or external API key is required.

## Run locally

```sh
make run-demo
```

Open http://127.0.0.1:8091. Stop an older console using that port, or choose another with `DEMO_ADDR=127.0.0.1:8105 make run-demo`. No `.env` setup is required for the demo.

## Deploy on Render

1. Push the updated source to your GitHub repository.
2. Create a Render Blueprint from the repository. The included `render.yaml` requests a free Docker web service named `reliable-webhook-demo`. If the project is inside a larger repository, set its root directory to the folder containing this Dockerfile.
3. Set `PUBLIC_URL` to the exact HTTPS origin assigned to your service, such as `https://YOUR_SERVICE.onrender.com`, with no trailing slash. Render supplies `PORT`. If you only learn the URL after creation, update the variable and redeploy.
4. Wait for the deployment and `/health/ready` check to pass, then open the service URL.

The deployment needs only `PUBLIC_URL`; do not copy your local `.env` or database credentials into it. Confirm Render offers a free plan before creating the service; this setup does not require a paid service. Hosting availability and limits are controlled by the provider.

## What a visitor can do

A workspace starts automatically without login. **Send sample event** publishes `order.created` to a real internal HTTP receiver. **Try a retry** publishes `demo.retry`: the receiver returns HTTP 503 once, then HTTP 204. Inspect the delivery to see both attempts. Visitors can also publish their own small sample JSON events, browse events, and inspect deliveries.

Destinations are fixed to the internal receiver. Creating external destinations, changing secrets, and replaying deliveries are disabled in this playground. No requests are sent to visitor-supplied URLs.

## Temporary data and bounds

- Workspaces expire after 30 minutes; all demo data disappears on process restart.
- Each workspace accepts up to 10 events, each at most 16 KiB.
- The process supports up to 128 active workspaces and 20 new sessions per minute, with bounded delivery concurrency and queue size.
- Each visitor gets an opaque HttpOnly session cookie and server-side workspace isolation. Mutations require CSRF protection. Hosted cookies require HTTPS.
- **End demo** deletes that visitor’s workspace. An expired workspace can be replaced with **Start a fresh demo**.

Use sample data only. Free hosting may stop or restart the process; this is a portfolio playground, not an always-on durable webhook service. The full PostgreSQL backend remains available through the main README. The older optional account-based setup is in [deploy-accounts.md](deploy-accounts.md).
