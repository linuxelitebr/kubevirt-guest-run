# kubevirt-guest-run

Run a command, or a whole script, inside a KubeVirt VM through the
`qemu-guest-agent`, without depending on the guest's network. The channel is
virtio-serial: the same thing that's still standing when the VM's network is
down, SSH won't connect and RDP won't either.

Underneath it's a `guest-exec` fired from inside the `virt-launcher` pod. The
pid/polling/base64 dance stays hidden. For a long Windows script it wraps the
whole thing in PowerShell's `-EncodedCommand`, which is the only way to send
quotes, newlines and non-ASCII without the command-line parser mangling it on
the way in. Ask me how I know.

## What it does

- Finds the VM's `virt-launcher` pod on its own.
- Builds the `guest-exec`, fires it, waits for it to finish, and hands you the
  output already decoded.
- Exits with the exit code of the process inside the guest, so you can use it in
  a script.
- Shells out to `oc` (or `kubectl`), so it respects your kubeconfig, context and
  auth. One binary, zero dependency on `jq`, `iconv`, or whichever `base64` your
  OS happened to ship.

## What it does not do

- No interactive shell. It's one command at a time, output back. For a real
  session you want the console, or SSH once the network is back.
- It does not get around SELinux. On Linux the agent runs confined in the
  `virt_qemu_ga_t` domain, so `systemctl`, reading a protected log, and a handful
  of tools are blocked. That's by design, and this binary is not what changes it.

## Install

Grab a prebuilt binary from the [releases page](https://github.com/linuxelitebr/kubevirt-guest-run/releases):
one archive per platform (Linux, macOS, Windows; amd64 and arm64), each with the
binary, this README and the license. Unpack it and drop `guest-run` on your PATH.

Or build it yourself:

```bash
go build -o guest-run .
# or
go install github.com/linuxelitebr/kubevirt-guest-run@latest
```

You need `oc` or `kubectl` on your PATH, and `exec` access to the VM's pod. On
what that access actually means, read the security section below. It's not a
small thing.

## Usage

```
guest-run -vm NAME [options] <mode>
```

Modes, pick one:

| Mode | What it does |
| --- | --- |
| `-ps "COMMAND"` | PowerShell inline, via `-EncodedCommand` (Windows) |
| `-ps-file FILE` | PowerShell script from a file, via `-EncodedCommand` (`-` = stdin) |
| `-sh "COMMAND"` | `/bin/sh -c` in the guest (Linux) |
| `-- PATH [ARGS]` | exec a binary directly, no shell (any OS) |

Options that matter:

| Option | Default | For |
| --- | --- | --- |
| `-vm NAME` | (required) | VM name |
| `-n NS` | `default` | VM namespace |
| `-context CTX` | current context | oc/kubectl context |
| `-timeout DUR` | `60s` | max time to wait for completion |
| `-interval DUR` | `1s` | poll interval for the status check |
| `-kubectl` | uses oc | use kubectl instead of oc |
| `-raw` | decodes | print the raw status JSON, without decoding |
| `-force` | checks | skip the agent-connected check |

## Examples

```bash
# Windows: see the network config
guest-run -vm win01 -ps 'Get-NetIPConfiguration | Out-String'

# Windows: run a whole script, with quotes and non-ASCII, no drama
guest-run -vm win01 -ps-file fix-network.ps1

# Windows: re-enable a hidden adapter that walked off with the static IP
guest-run -vm win01 -ps 'Get-NetAdapter | Where-Object Status -ne Up | Enable-NetAdapter -Confirm:$false'

# Linux: the basics, through a shell
guest-run -vm rhel01 -sh 'ip -br a; systemctl is-active chronyd || true'

# Linux: a binary directly, through no shell at all
guest-run -vm rhel01 -- /usr/bin/id -un

# From stdin, to build the script on the fly
printf 'Write-Output $env:COMPUTERNAME' | guest-run -vm win01 -ps-file -
```

`guest-run`'s exit code is the exit code of the process inside the guest, so
`guest-run ... && echo it-worked` behaves the way you'd expect.

## Honest gotchas

**PATH resolves, but SELinux has the final say.** A bare `whoami` or `id`, no
path, runs fine: `guest-exec` searches PATH, just like a shell. So when something
fails, don't go slapping the full path on it thinking that fixes it. If the error
is `Permission denied`, the binary was found and the execution was blocked, and
on Linux that's almost always SELinux. `No such file or directory` is actually
not-found. The absolute path changes nothing in the first case.

**On RHEL, `guest-exec` may ship disabled.** Red Hat's docs turn `guest-exec` off
by default on some images. If the response says the command isn't available, the
channel is closed in the guest, and reopening it usually takes... access to the
VM. The chicken eyes the egg. Enable it in your base images while the VM is still
healthy.

**PowerShell output.** `guest-run` already forces output to UTF-8 and silences
the progress stream before running your script, so text comes back readable and
stderr isn't polluted with CLIXML. A real error your script throws can still come
back serialized on stderr. That's PowerShell being PowerShell.

**Live migration.** If the VM is migrating at that exact moment, there can be more
than one Running `virt-launcher` pod for an instant. `guest-run` takes the first.
In steady state there's only one.

## Security: whoever runs this is SYSTEM

This is not a footnote. On Windows the `qemu-guest-agent` runs as
`NT AUTHORITY\SYSTEM`, so every command you send through here is born SYSTEM: no
login, no password, no session trace inside the guest. On Linux it runs as root,
SELinux-confined, but root all the same.

And the gate to this channel is `create` on `pods/exec`, which the default `edit`
and `admin` roles on OpenShift already carry. Meaning: anyone with `edit` in a
namespace that hosts Windows VMs is SYSTEM on all of them, and KubeVirt's own VM
access permissions (console, VNC) don't cover this path, because it goes down
through the pod. Treat `create pods/exec` in those namespaces as a privileged
grant.

The full story, with the measurements, is in the post:
https://linuxelite.com.br/blog/guest-exec-vm-no-network/

## Releasing

Builds happen locally, not in CI (this org keeps GitHub Actions billing off).
`release.sh` cross-compiles every supported platform, packages one archive per
platform plus a `SHA256SUMS`, and can publish the GitHub release:

```bash
./release.sh            # build + package into dist/
./release.sh --publish  # the above, then create the release (needs gh)
```

The version comes from the `version` const in `main.go`. Bump it there, run the
script. No CI, no tags to babysit.

## License

Apache-2.0.
