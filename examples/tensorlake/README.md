# Tensorlake cloud sandbox example

This example runs the agent inside a real [Tensorlake](https://docs.tensorlake.ai/sandboxes/introduction)
cloud sandbox. The host process stays on your machine (the model provider,
secrets, broker and audit); Tensorlake runs the VM, and kiln connects to it
through Tensorlake's process API.

## Requirements

- A Tensorlake API key: create one from the
  [sandbox quickstart](https://docs.tensorlake.ai/sandboxes/quickstart).
- Go 1.27+.

## Run

```bash
export TENSORLAKE_API_KEY="..."
go run ./examples/tensorlake
```

The host builds `kiln` for `linux/amd64` (the default Tensorlake Ubuntu image is
x86_64), creates an ephemeral cloud sandbox, uploads the box program (with its compiled modules),
starts `kiln serve --stdio`, and deletes the sandbox on exit.

If your Tensorlake image is arm64, set `TL_GUEST_ARCH=arm64`.

## What runs where

```
your machine / server (the host)
  ├── kiln host process: model provider, secrets, broker, audit
  └── Tensorlake cloud sandbox
        └── the box program (ServeBox) + modules + tools
```

The host knows the sandbox through the Tensorlake API. The sandbox does not
know the host: the host starts the process and holds its stdin/stdout. Every
`host.secret`, `host.http` and `host.llm` request rides that stream, so the
real secret never enters Tensorlake.

## Sandbox settings

| Variable | Default |
| --- | --- |
| `TENSORLAKE_API_KEY` | required |
| `TL_GUEST_ARCH` | `amd64` |
| `TL_IMAGE` | `tensorlake/ubuntu-minimal` |
| `TL_WORKDIR` | `/home/tl-user` |
| `TL_TIMEOUT_SECS` | `900` |

The `sandbox_tensorlake` module also accepts `image`, `workdir`,
`timeout_secs`, `api_key`, `sandbox_api` and `sandbox_proxy` config keys.

The example maps `RunIn.Spec.Network.Mode` to Tensorlake network settings:
`none` disables internet access, `allowlist` uses `AllowOut`, and `full` allows
internet access. With `WithRunIn`, the box's HTTP is relayed through the host
regardless, so the example still works with `none`.
