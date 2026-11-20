# Vision (LAN image-to-text)

`vision` is a small CLI that sends one image to the LAN vision model and
prints its text answer. It wraps ollama's `/api/chat` (`stream=false`, image
base64-encoded) via `internal/vision`.

- Endpoint: `http://192.168.1.147:11435` (ollama on openclaw)
- Model: `qwen2.5vl:7b` (Q4_K_M)
- Override with env `FACTORY_VISION_URL` / `FACTORY_VISION_MODEL`.

## Using it from a job

```sh
vision /path/to/screenshot.png
vision --prompt "Read all text in this image verbatim." ./scan.jpg
vision --prompt "What does the chart show?" https://example.com/chart.png
```

The argument is a file path or an `http(s)` URL (max 20 MiB). The answer goes
to stdout; errors go to stderr. `--timeout` (default 120s) bounds the model call.

Exit status: `0` ok, `1` error, `2` usage, `3` endpoint unreachable
(typically the foreman is not on the LAN; the endpoint is LAN-only).

## Limits

- ~7 s per image. The GPU is shared with live captions/Whisper, so keep calls
  **serial**: one at a time, no parallel fan-out. The CLI takes a per-host lock
  (`$TMPDIR/factory-vision.lock`) so concurrent jobs on one machine queue;
  it cannot coordinate across machines.
- Good at captions and clear-text OCR. Weak on chart values and cropped or
  tiny text: treat numbers read from charts as unverified.
- LAN-only; not part of agent-cloud. Foremen outside the LAN get exit 3.
- It is a 7B model: verify anything that matters.

## Testing

`go test ./internal/vision ./cmd/vision` uses a fake server. One opt-in live
call: `FACTORY_VISION_LIVE=1 go test ./internal/vision -run Live -v`.
