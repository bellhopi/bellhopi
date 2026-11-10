# Local verification — 2026-11-10

The Go implementation and shared Codex/Claude Code skill passed local verification. No Telnyx or OpenAI provider API calls were made, no call was placed, and neither client's actual skill directory was changed.

| Check | Result |
| --- | --- |
| `go test ./... -count=1 -timeout=60s` | Both packages passed |
| `go test -race ./... -count=1 -timeout=90s` | Both packages passed |
| `go vet ./...` | Passed |
| `make build` | Built `bin/concierge-dialer` for the local macOS ARM64 environment |
| Skill creator `quick_validate.py skills/concierge-dialer` | Skill is valid |
| Independent specification and focused code review | Signed off after fixes |
| Shared skill scenario walkthrough | Passed; source and instructions only |

Fake-provider tests cover signed callbacks and media, audio forwarding, tool batching, DTMF validation and duplicate commands, unknown command outcomes, durable call reservation, restart recovery, question replies, model failure during dialing, failed receipt writes, and shutdown races. These checks exercise local protocol handling; they do not establish compatibility with live accounts or IVR recognition.

The final executable passed a separate smoke check: help, private configuration creation, missing-field doctor failure, configuration overwrite refusal, installation into temporary Codex and Claude Code homes, matching installed files, installation overwrite refusal, reinstallation from a packaged skill into another temporary home, localhost health, operator authentication, and clean SIGTERM shutdown with no active call.

Initial local validation executable SHA-256 (before the GitHub module path was set):

```text
fc4a8d8a6fdb701a98c61f3589286d46e07f5a808fc0b9b3e276c1fb8719631d
```

The smoke receipt and log are retained privately outside the source tree. The temporary configuration contains only fake provider credentials and its generated local token.

## GitHub publication preparation

After setting the module path to `github.com/bellhopi/bellhopi`, `go test -race ./... -count=1 -timeout=90s`, `go vet ./...`, `make build`, and skill validation passed again. Local documentation links and Claude attribution JSON were checked.

The resulting executable passed private configuration creation, missing/configured `doctor` checks, temporary installation into both clients with the MIT license, installed executable help, localhost health, and clean shutdown. This used dummy credentials and no provider calls. Its SHA-256 is:

```text
afa99981f6e4df3d030b2e375e9b0b2498a6e215d00ab9de462557d82a46471a
```

## Outstanding live verification

Provide the user's Telnyx/OpenAI credentials, a reachable HTTPS/WSS endpoint, and an authorized test destination. Observe audio in both directions, an accepted DTMF menu selection, dialogue with the intended recipient, the task outcome, and confirmed hangup. `doctor` validates local fields only. A successful provider command response does not prove the IVR accepted the digit or that a business transaction completed.
