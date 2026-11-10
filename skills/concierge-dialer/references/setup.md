# Setup and operation

Use the skill's installed `bin/concierge-dialer`. If building from source, the repository README gives the build and installation commands. Installation is separate from provider configuration and never places a call.

## Private configuration

Run:

```sh
<skill-dir>/bin/concierge-dialer init
<skill-dir>/bin/concierge-dialer doctor
```

The default configuration is `${XDG_CONFIG_HOME:-$HOME/.config}/concierge-dialer/config.json`. `init` creates a private file and random local API token, and refuses to overwrite existing configuration. To use another path, pass `--config /absolute/path/config.json` **before** every command.

Have the user enter their credentials using a local editor or their environment's secret mechanism. Do not ask for API keys in conversation, place them in task files, commit them, pass them in CLI arguments, or print the configuration. Keep the file mode at `0600` and its directory private.

| Configuration field | Value |
| --- | --- |
| `openai_api_key` | User's OpenAI API key with access to the configured models |
| `telnyx_api_key` | User's Telnyx API key |
| `telnyx_public_key` | Base64 Ed25519 public key used to verify Telnyx signatures |
| `telnyx_connection_id` | Telnyx Call Control connection/application ID |
| `telnyx_from_number` | User's configured Telnyx outbound number in E.164 format |
| `public_base_url` | Public HTTPS origin routing to the local service |
| `api_token` | Generated local operator token; retain at least 32 characters |
| `listen` | Loopback address, default `127.0.0.1:8787` |
| `state_dir` | Private persistent receipt directory created beside the configuration by default |
| `live_model` | Defaults to `gpt-live-1` |
| `backend_model` | Defaults to `gpt-6-luna` |
| `voice` | Defaults to `marin` |
| `max_call_seconds` | Provider duration limit, default `600`; supported range `30`–`14400` |

Environment overrides are `OPENAI_API_KEY`, `TELNYX_API_KEY`, `TELNYX_PUBLIC_KEY`, `TELNYX_CONNECTION_ID`, `TELNYX_FROM_NUMBER`, `PUBLIC_BASE_URL`, and `CONCIERGE_API_TOKEN`. Nonempty environment values override the corresponding file values. The service and CLI must see the same operator token.

`doctor` reports missing or invalid local fields and exits nonzero while any remain. A passing result says nothing about provider permissions, model access, reachable callbacks, or audible phone conversations. No call is placed by `init`, `doctor`, or `serve`.

## Public callbacks and media

Run a persistent HTTPS tunnel or reverse proxy to the loopback listener. Configure `public_base_url` as the HTTPS origin, for example `https://your-dialer.example`, without a path or query. The proxy must support WebSocket upgrades.

- Telnyx event callback: `POST /webhooks/telnyx`.
- Telnyx media: WebSocket upgrade at `/media/{call-id}` using the complete service-generated stream URL, including its query parameters.
- Public liveness endpoint: `GET /health`.
- Operator routes: `/v1/calls` and `/v1/calls/{call-id}/...`, authenticated with the generated bearer token. CLI requests always go to the configured loopback listener; they do not use `public_base_url`.

Preserve the raw event body, `telnyx-signature-ed25519`, and `telnyx-timestamp` headers through the proxy, including media upgrade requests. Both valid signatures and fresh timestamps are mandatory. Do not disable these checks when diagnosing a connection failure. Configure the Telnyx Call Control application for outbound calling and the event callback above. Keep the service and tunnel running throughout calls.

The audio bridge uses PCMU, mono, 8 kHz, with Telnyx bidirectional streaming and GPT-Live. The service sends provider duration limits, makes bounded requests, and attempts hangup when shut down. An unknown hangup response still requires status/reconciliation. Audio is not recorded by this service.

## Local evidence and recovery

Receipts reserve the stable request ID before dialing and persist provider identifiers and limited call state in private files. A repeated request with the same ID refers to the original call. Keep the same task content with that ID; do not delete receipts as a retry mechanism. Transcripts are in memory and disappear when the service exits; receipts are not transcript archives.

On restart, an unfinished receipt becomes `recovery_required`. Use `status`, then `reconcile` or `hangup` for that ID. A provider error or timeout must not trigger an automatic replacement call. If provider status remains unknown, preserve the receipt and tell the user what could not be established.

The first live call needs an actual user-authorized destination and completed setup. Verify connection events, dialogue in both directions, menu acceptance, the intended outcome, and final hangup from actual call evidence. Passing local tests does not establish those observations.
