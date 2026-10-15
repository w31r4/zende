# Local patch

Source: `github.com/muesli/cancelreader v0.2.2`, MIT license in `LICENSE`.
Root `go.mod` replaces that module with this directory. Other platforms are
unchanged; Zende's supported runtime is Linux/WSL.

Linux changes:

- `Close` cancels and joins an active `Read` before releasing descriptors.
- Repeated `Cancel`/`Close` are idempotent and safe concurrently.
- `Cancel` also releases the joined reader: Bubble Tea replaces it on terminal
  handoff without calling `Close` on the old instance.
- Failed epoll setup closes both pipe ends; epoll is close-on-exec.
- Use a constant `fmt.Errorf` format for current Go vet.

Regression: `internal/render/input_reader_linux_test.go` plus real-terminal
detach, cancellation, panic and reconnect tests. Do not remove the replacement
until an upstream version passes these race tests. Shared module caches and
research checkouts are unchanged.
