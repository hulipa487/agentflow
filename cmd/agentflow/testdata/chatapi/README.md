# OpenAI channel fixtures

Request/response pairs replayed by the `internal/drivers/chatapi` tests.

The files named `handwritten-*` were written from the OpenAI Chat Completions
API reference, not captured from a client. They cover the six cases phase 0.3
requires: plain chat, streaming chat, a request with tools, a follow-up carrying
a `tool` result, an image part, and `stream_options.include_usage`.

A real capture is written by the recorder:

    go run ./internal/drivers/chatapi/cmd/record \
      -upstream https://api.openai.com/v1 \
      -out cmd/agentflow/testdata/chatapi

Point a client's `base_url` at the recorder and drive the client through the
scenarios the compatibility list names. Captures are committed; the recorder
redacts the `Authorization` header, API-key headers, secret-named JSON fields
and key-shaped strings, but the file is still worth reading before it is
committed.

**Do not claim compatibility with a client that has no captured fixture.**
