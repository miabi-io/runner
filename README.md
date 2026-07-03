# miabi-runner

Machine-side runtime for Miabi's **dedicated build & pipeline execution**. A
runner is a build machine: it dials the Miabi control plane over an outbound,
NAT-friendly WebSocket, authenticates with its registration token, and appears
online — so builds and pipelines run here instead of on the app/database hosting
nodes. A hosting node then only ever *pulls* the resulting image, never builds it.

Unlike the [node agent](https://github.com/miabi-io/agent), a runner exposes **no
Docker socket** to the control plane — it uses its own local Docker/BuildKit
daemon. The tunnel exists so the control plane can lease build jobs to it and
stream logs/status back.


## Run

Register a runner in the Miabi UI (**Settings → Runners → Add runner**, or
**Admin → Runners** for a platform-shared one) to get a one-time token, then:

```sh
docker run -d --name miabi-runner \
  -e MIABI_CONTROL_URL=https://panel.example.com \
  -e MIABI_RUNNER_TOKEN=mbr_xxxxxxxx \
  miabi/runner:latest
```

Or as a binary: `MIABI_CONTROL_URL=… MIABI_RUNNER_TOKEN=… ./miabi-runner`.

## Configuration (environment)

| Variable | Required | Meaning |
|---|---|---|
| `MIABI_CONTROL_URL` | yes | Control plane base URL (falls back to `MIABI_API_URL`) |
| `MIABI_RUNNER_TOKEN` | yes | Registration token issued when the runner was added (`mbr_…`) |
| `MIABI_RUNNER_INSECURE_SKIP_VERIFY` | no | Skip TLS verification of the control plane (dev only; default false) |
| `MIABI_RUNNER_BUILDER` | no | Build backend: `docker` (default; also runs container steps) or `buildkit` (rootless/daemonless, build-only, no docker.sock) |
| `MIABI_DEV_MODE` | no | Debug logging (default false) |

The runner reports its OS/arch/version to the control plane on connect (used for
label/arch job scheduling). Licensed under Apache-2.0.
