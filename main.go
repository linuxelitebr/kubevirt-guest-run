// Command guest-run runs a command or a whole script inside a KubeVirt VM
// through the qemu-guest-agent, without depending on the guest's network.
//
// The channel is virtio-serial: the same thing that saves you when the VM's
// network is down and SSH won't connect. Underneath it's a guest-exec fired
// from inside the virt-launcher pod, with the whole pid/polling/base64 dance
// hidden away.
//
// It shells out to oc (or kubectl), so it respects your kubeconfig, context and
// auth with no client-go involved. One binary, zero dependency on jq, iconv, or
// whichever base64 your OS happened to ship.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf16"
)

const version = "0.1.0"

// usage prints on -h and on a usage error. Works with no oc and no cluster.
func usage() {
	fmt.Fprint(os.Stderr, `guest-run `+version+` - run a command/script inside a KubeVirt VM through the guest agent

USAGE
  guest-run -vm NAME [options] <mode>

MODES (pick one)
  -ps "COMMAND"        PowerShell inline, via -EncodedCommand (Windows)
  -ps-file FILE        PowerShell script from a file, via -EncodedCommand (- = stdin)
  -sh "COMMAND"        /bin/sh -c in the guest (Linux)
  -- PATH [ARGS]       exec a binary directly, no shell (any OS)

OPTIONS
  -vm NAME             VM name (required)
  -n, -namespace NS    VM namespace (default: default)
  -context CTX         oc/kubectl context
  -timeout DUR         max time to wait for completion (default 60s)
  -interval DUR        poll interval for the status check (default 1s)
  -kubectl             use kubectl instead of oc
  -raw                 print the raw guest-exec-status JSON, without decoding
  -force               do not check that the guest agent is connected first
  -verbose             log each step (pod, dispatch, polling) to stderr
  -h, -help            this help

EXAMPLES
  guest-run -vm win01 -ps 'Get-NetIPConfiguration | Out-String'
  guest-run -vm win01 -ps-file fix-network.ps1
  guest-run -vm rhel01 -sh 'ip -br a; systemctl is-active chronyd'
  guest-run -vm rhel01 -- /usr/bin/id -un
  echo '$env:COMPUTERNAME' | guest-run -vm win01 -ps-file -

guest-run's exit code is the exit code of the process inside the guest.
`)
}

// opts holds everything the command line configures.
type opts struct {
	vm         string
	namespace  string
	context    string
	timeout    time.Duration
	interval   time.Duration
	useKubectl bool
	raw        bool
	force      bool
	verbose    bool
}

func main() {
	o := &opts{namespace: "default", timeout: 60 * time.Second, interval: time.Second}
	var psInline, psFile, shInline string
	var showHelp, showVersion bool

	// Hand-rolled parser: Go's flag package dislikes mixing flags with a
	// trailing "-- PATH ARGS" positional, and we want both.
	args := os.Args[1:]
	var rest []string // whatever comes after "--"
	i := 0
	need := func(flag string) string {
		i++
		if i >= len(args) {
			fail("missing value for %s", flag)
		}
		return args[i]
	}
	for ; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-h", "-help", "--help":
			showHelp = true
		case "-version", "--version":
			showVersion = true
		case "-vm", "--vm":
			o.vm = need(a)
		case "-n", "-namespace", "--namespace":
			o.namespace = need(a)
		case "-context", "--context":
			o.context = need(a)
		case "-timeout", "--timeout":
			o.timeout = mustDur(need(a))
		case "-interval", "--interval":
			o.interval = mustDur(need(a))
		case "-kubectl", "--kubectl":
			o.useKubectl = true
		case "-raw", "--raw":
			o.raw = true
		case "-force", "--force":
			o.force = true
		case "-verbose", "--verbose":
			o.verbose = true
		case "-ps", "--ps":
			psInline = need(a)
		case "-ps-file", "--ps-file":
			psFile = need(a)
		case "-sh", "--sh":
			shInline = need(a)
		case "--":
			rest = args[i+1:]
			i = len(args)
		default:
			fail("unknown option: %s (try -h)", a)
		}
	}

	if showVersion {
		fmt.Println("guest-run", version)
		return
	}
	if showHelp {
		usage()
		return
	}
	if o.vm == "" {
		usage()
		fail("missing -vm")
	}

	// Build the guest-exec payload from the chosen mode. One mode at a time.
	path, cmdArgs, err := buildCommand(psInline, psFile, shInline, rest)
	if err != nil {
		fail("%v", err)
	}

	if err := run(o, path, cmdArgs); err != nil {
		fail("%v", err)
	}
}

// buildCommand decides what actually runs in the guest, based on the mode.
func buildCommand(psInline, psFile, shInline string, rest []string) (string, []string, error) {
	modes := 0
	for _, on := range []bool{psInline != "", psFile != "", shInline != "", len(rest) > 0} {
		if on {
			modes++
		}
	}
	if modes == 0 {
		return "", nil, fmt.Errorf("pick a mode: -ps, -ps-file, -sh or -- PATH (try -h)")
	}
	if modes > 1 {
		return "", nil, fmt.Errorf("pick only one mode at a time")
	}

	switch {
	case psInline != "":
		return powershell(psInline)
	case psFile != "":
		script, err := readScript(psFile)
		if err != nil {
			return "", nil, err
		}
		return powershell(script)
	case shInline != "":
		return "/bin/sh", []string{"-c", shInline}, nil
	default:
		return rest[0], rest[1:], nil
	}
}

// powershell wraps a script in -EncodedCommand: base64 of UTF-16LE, which is the
// only way to send a script with quotes, newlines and non-ASCII without the
// Windows command-line parser mangling it along the way.
//
// We force the output to UTF-8 before running your script, otherwise what comes
// back is in the console code page and reaches you as mojibake.
func powershell(script string) (string, []string, error) {
	// Silent ProgressPreference drops the "Preparing modules for first use"
	// that PS serializes as CLIXML onto stderr. UTF8Encoding with no BOM so the
	// output comes back readable instead of in the console code page.
	prelude := "$ProgressPreference='SilentlyContinue'; " +
		"$OutputEncoding = [Console]::OutputEncoding = New-Object System.Text.UTF8Encoding $false; "
	enc := encodeUTF16LE(prelude + script)
	// Bare powershell.exe resolves via the agent's PATH (it runs as SYSTEM, so
	// it sees the 64-bit System32). No need for the full path.
	return "powershell.exe", []string{"-NoProfile", "-NonInteractive", "-EncodedCommand", enc}, nil
}

// encodeUTF16LE produces what PowerShell's -EncodedCommand expects: the
// UTF-16 little-endian bytes of the text, base64-encoded.
func encodeUTF16LE(s string) string {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 0, len(u)*2)
	for _, r := range u {
		b = append(b, byte(r), byte(r>>8))
	}
	return base64.StdEncoding.EncodeToString(b)
}

func readScript(path string) (string, error) {
	if path == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("reading script from stdin: %w", err)
		}
		return string(b), nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	return string(b), nil
}

// --- talking to the cluster -------------------------------------------------

// guestExecReq is the JSON for the guest-exec command.
type guestExecReq struct {
	Execute   string `json:"execute"`
	Arguments struct {
		Path          string   `json:"path"`
		Arg           []string `json:"arg"`
		CaptureOutput bool     `json:"capture-output"`
	} `json:"arguments"`
}

type execResp struct {
	Return struct {
		Pid int `json:"pid"`
	} `json:"return"`
}

type statusResp struct {
	Return struct {
		Exited   bool   `json:"exited"`
		ExitCode *int   `json:"exitcode"`
		Signal   *int   `json:"signal"`
		OutData  string `json:"out-data"`
		ErrData  string `json:"err-data"`
		OutTrunc bool   `json:"out-truncated"`
		ErrTrunc bool   `json:"err-truncated"`
	} `json:"return"`
}

func run(o *opts, path string, cmdArgs []string) error {
	domain := o.namespace + "_" + o.vm

	if !o.force {
		if err := checkAgent(o); err != nil {
			return err
		}
	}

	pod, err := resolvePod(o)
	if err != nil {
		return err
	}
	o.logf("pod: %s", pod)

	// Build and fire the guest-exec.
	var req guestExecReq
	req.Execute = "guest-exec"
	req.Arguments.Path = path
	req.Arguments.Arg = cmdArgs
	req.Arguments.CaptureOutput = true
	reqJSON, _ := json.Marshal(req)
	o.logf("guest-exec: path=%s arg=%v", path, cmdArgs)

	out, stderr, err := o.agentCommand(pod, domain, string(reqJSON))
	if err != nil {
		return fmt.Errorf("firing guest-exec: %s", firstLine(stderr, err))
	}
	var er execResp
	if e := json.Unmarshal([]byte(out), &er); e != nil || er.Return.Pid == 0 {
		return fmt.Errorf("unexpected guest-exec response: %s", strings.TrimSpace(out+stderr))
	}
	o.logf("pid: %d", er.Return.Pid)

	// Poll the status until it exits or the timeout runs out.
	statusReq := fmt.Sprintf(`{"execute":"guest-exec-status","arguments":{"pid":%d}}`, er.Return.Pid)
	deadline := time.Now().Add(o.timeout)
	for {
		sOut, sErr, e := o.agentCommand(pod, domain, statusReq)
		if e != nil {
			return fmt.Errorf("querying status: %s", firstLine(sErr, e))
		}
		var sr statusResp
		if e := json.Unmarshal([]byte(sOut), &sr); e != nil {
			return fmt.Errorf("unreadable status: %s", strings.TrimSpace(sOut))
		}
		if sr.Return.Exited {
			return finish(o, sr, sOut)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting on pid %d (still running)", o.timeout, er.Return.Pid)
		}
		time.Sleep(o.interval)
	}
}

// finish decodes the output and exits with the guest process's code.
func finish(o *opts, sr statusResp, rawJSON string) error {
	if o.raw {
		fmt.Println(strings.TrimSpace(rawJSON))
	} else {
		if b, err := base64.StdEncoding.DecodeString(sr.Return.OutData); err == nil {
			os.Stdout.Write(b)
		}
		if b, err := base64.StdEncoding.DecodeString(sr.Return.ErrData); err == nil {
			os.Stderr.Write(b)
		}
	}
	if sr.Return.OutTrunc {
		fmt.Fprintln(os.Stderr, "guest-run: warning: stdout truncated by the agent")
	}
	if sr.Return.ErrTrunc {
		fmt.Fprintln(os.Stderr, "guest-run: warning: stderr truncated by the agent")
	}
	code := 0
	switch {
	case sr.Return.ExitCode != nil:
		code = *sr.Return.ExitCode
	case sr.Return.Signal != nil:
		code = 128 + *sr.Return.Signal
	}
	if code != 0 {
		os.Exit(code)
	}
	return nil
}

// --- oc/kubectl ------------------------------------------------------------

func (o *opts) cli() string {
	if o.useKubectl {
		return "kubectl"
	}
	return "oc"
}

// base returns the common args (context), for every command.
func (o *opts) base() []string {
	if o.context != "" {
		return []string{"--context", o.context}
	}
	return nil
}

// agentCommand runs: oc exec -n NS POD -c compute -- virsh -c qemu:///session
// qemu-agent-command DOMAIN <json>
func (o *opts) agentCommand(pod, domain, payload string) (string, string, error) {
	a := append(o.base(), "exec", "-n", o.namespace, pod, "-c", "compute", "--",
		"virsh", "-c", "qemu:///session", "qemu-agent-command", domain, payload)
	return runCLI(o.cli(), a)
}

// resolvePod finds the Running virt-launcher pod for the VM.
func resolvePod(o *opts) (string, error) {
	a := append(o.base(), "get", "pod", "-n", o.namespace,
		"-l", "vm.kubevirt.io/name="+o.vm,
		"--field-selector=status.phase=Running",
		"-o", "jsonpath={.items[0].metadata.name}")
	out, stderr, err := runCLI(o.cli(), a)
	if err != nil {
		return "", fmt.Errorf("finding the pod for VM %q: %s", o.vm, firstLine(stderr, err))
	}
	pod := strings.TrimSpace(out)
	if pod == "" {
		return "", fmt.Errorf("no Running virt-launcher pod for VM %q in namespace %q", o.vm, o.namespace)
	}
	return pod, nil
}

// checkAgent confirms the guest agent answered, so we give a clear error instead
// of virsh's raw one when the VM has no agent.
func checkAgent(o *opts) error {
	a := append(o.base(), "get", "vmi", o.vm, "-n", o.namespace,
		"-o", `jsonpath={.status.conditions[?(@.type=="AgentConnected")].status}`)
	out, stderr, err := runCLI(o.cli(), a)
	if err != nil {
		return fmt.Errorf("checking VMI %q: %s", o.vm, firstLine(stderr, err))
	}
	if strings.TrimSpace(out) != "True" {
		return fmt.Errorf("the guest agent for VM %q is not connected (use -force to try anyway)", o.vm)
	}
	return nil
}

func runCLI(bin string, args []string) (string, string, error) {
	cmd := exec.Command(bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

// --- helpers ---------------------------------------------------------------

func (o *opts) logf(format string, a ...any) {
	if o.verbose {
		fmt.Fprintf(os.Stderr, "guest-run: "+format+"\n", a...)
	}
}

func firstLine(stderr string, err error) string {
	s := strings.TrimSpace(stderr)
	if s != "" {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[:i]
		}
		return s
	}
	return err.Error()
}

func mustDur(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		fail("invalid duration %q: %v", s, err)
	}
	return d
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "guest-run: "+format+"\n", a...)
	os.Exit(2)
}
