# Bellhopi concierge dialer skill

The revised scope is a reusable skill for Codex and Claude Code, implemented in Go with github.com/enetx/g. A request such as “Call UPS and get a mailbox; here is my information” is a general task, not a fixed IVR script.

The host agent resolves the business/branch and its authoritative phone number, asks for genuinely missing facts, and writes a private JSON request. One SKILL.md guides both clients. A Go CLI starts a service, submits calls, reads progress and transcripts, supplies answers, sends manual DTMF, and ends calls. Each user supplies their own Telnyx and OpenAI credentials. There is no browser app.

The service uses Telnyx Call Control and signed bidirectional media. GPT-Live (gpt-live-1) converses using mono PCMU at 8 kHz. Responses delegation (gpt-6-luna by default) has validated tools for DTMF, bounded waiting, asking the user, and reporting the outcome. It navigates one menu level at a time and continues the task with staff. Unknown facts and new decisions are returned to the host agent.

One active call is supported. Credentials stay in private configuration or environment. Operator endpoints require a token; Telnyx callbacks/media require Ed25519 signatures and fresh timestamps. Private receipts reserve stable request IDs before dialing and retain provider identifiers and unknown outcomes across restarts. Restored active calls require reconciliation or hangup, never redial. Audio is not recorded. Transcripts remain in memory; limited call state and outcomes are private local files.

The installer copies the same skill and binary into either client's skill directory without overwriting. This build does not install globally or place calls. Provider duration limits and failure cleanup bound abandoned calls.

Tests cover signatures, deduplication, event ordering, tool arguments, questions, PCMU forwarding, Responses continuation, unknown outcomes, restart recovery, CLI commands, and installation in temporary directories. Native tests do not establish caller-heard audio or remote IVR acceptance.

Verified references:
- https://github.com/enetx/g
- https://developers.openai.com/api/docs/guides/voice-websockets?api=live
- https://developers.openai.com/api/docs/guides/live-delegation
- https://developers.telnyx.com/docs/voice/programmable-voice/media-streaming
- https://developers.telnyx.com/api-reference/call-commands/dial
- https://developers.telnyx.com/api-reference/call-commands/send-dtmf
- https://code.claude.com/docs/en/skills
- https://learn.chatgpt.com/docs/build-skills
