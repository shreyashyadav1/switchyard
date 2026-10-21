# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- OpenAI-compatible `POST /v1/chat/completions` (streaming and non-streaming),
  `POST /v1/embeddings` and `GET /v1/models`; the same routes under `/openai/v1` for
  Groq-SDK clients.
- Named models mapped to ordered provider targets, with retries (jittered backoff,
  `Retry-After`) and fallback between targets.
- API keys stored as SHA-256 digests, per-key token-bucket rate limits and daily USD budgets.
- Cost accounting from provider `usage`, including streamed responses.
- Per-key exact-match LRU response cache for embeddings and `temperature: 0` chat requests.
- JSON access logs, Prometheus metrics on a private listener, and OpenTelemetry spans that
  continue the caller's W3C trace context.
- Distroless container image and a Terraform module for Google Cloud Run.
