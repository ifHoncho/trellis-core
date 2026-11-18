// trellis-deploy installs or upgrades Trellis on many nodes at once, from
// an operator's computer, over SSH. See docs/guide.md, "Install on many
// nodes at once".
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/deb"
	"meridian/trellis/internal/deploy"
	"meridian/trellis/internal/release"
)

var version = "dev"

const usage = `usage: trellis-deploy [options] NODES-FILE

Installs Trellis on the devices listed in NODES-FILE, or upgrades them,
from your computer over SSH. The first time:

  trellis-deploy --setup nodes.txt                    your SSH key on each device
  trellis-deploy --new-mesh team1 --country US        your mesh, in mesh.yaml
  trellis-deploy --profile mesh.yaml nodes.txt        install; they join the mesh

NODES-FILE has one device per line, user@address, optionally followed by
callsign=NAME, id=N (1-254), port=N (SSH) and mesh=ADDRESS (the node's mesh
address, to reach it through another listed node). The release is checked
before anything is done, the plan is shown, and the run asks once. See
docs/guide.md, and docs/reference.md for the details.

Options:
`

func main() {
	os.Exit(run())
}

func run() int {
	fs := flag.NewFlagSet("trellis-deploy", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage); fs.PrintDefaults() }
	rel := fs.String("release", "", "the release to deploy, as 0.2.0 (default: the newest published)")
	from := fs.String("from", "", "deploy the release files in this directory instead of downloading them")
	profilePath := fs.String("profile", "", "a mesh profile file (meshctl profile show on a node of the mesh) for nodes that have none: they join the mesh at once")
	webPass := fs.Bool("web-password", false, "ask for the web UI's admin password for new nodes even with --yes (asked anyway when the run installs a node)")
	noWebPass := fs.Bool("no-web-password", false, "do not ask for a web UI admin password for new nodes")
	newMesh := fs.String("new-mesh", "", "make a new mesh's profile (a fresh secret) named NAME, into --out, and stop")
	country := fs.String("country", "", "with --new-mesh: your two-letter country code, whose radio rules the mesh follows")
	band := fs.String("band", "2g", "with --new-mesh: 2g (every Wi-Fi radio has it) or 5g")
	outFile := fs.String("out", "mesh.yaml", "with --new-mesh: the profile file to write (never overwritten)")
	yes := fs.Bool("yes", false, "do not ask before going ahead")
	dry := fs.Bool("dry-run", false, "only check the release and show what would be done")
	jobs := fs.Int("jobs", 4, "new nodes installed at once")
	noReboot := fs.Bool("no-reboot", false, "do not reboot new nodes (they start Trellis at their next boot)")
	force := fs.Bool("force", false, "reinstall nodes that run this release already")
	keepGoing := fs.Bool("keep-going", false, "go on with the other upgrades after one goes back")
	trust := fs.String("trust", "", "also trust the release keys in this file (development builds)")
	nodeKey := fs.String("node-key", "", "make the nodes trust the release key in this file too (/etc/trellis/release-keys/deploy-dev.pub): a development key")
	withVoice := fs.Bool("voice", false, "also install push-to-talk voice (trellis-voice) on every node; nodes that have it are upgraded either way")
	fast := fs.Bool("no-health-check", false, "upgrade with apt, without meshctl upgrade's health check and rollback: faster, for development")
	anyVersion := fs.Bool("any-version", false, "install over a newer version too (development builds do not sort)")
	logs := fs.String("logs", "", "where each node's log goes (default: trellis-deploy-logs/<time>)")
	repo := fs.String("repo", deploy.DefaultRepo, "the GitHub repository releases come from")
	setup := fs.Bool("setup", false, "only ready the devices for later runs: your SSH key on each one it does not log in to yet (ssh-copy-id asks for that device's password)")
	sudoNoPass := fs.Bool("sudo-nopasswd", false, "with --setup: also let the login user use sudo without a password, so runs stop asking. Whoever logs in as that user with your key is then root on the device: see docs/reference.md before using it")
	sshKey := fs.String("ssh-key", "", "with --setup: the public key to install (default: ssh-copy-id's choice)")
	var sshOpts multi
	fs.Var(&sshOpts, "ssh-option", "an ssh option for every connection, as Name=value (repeatable)")
	showVersion := fs.Bool("version", false, "print the version")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Println(version)
		return 0
	}
	if *newMesh != "" {
		y, err := deploy.NewMesh(*newMesh, *country, *band)
		if err != nil {
			fmt.Fprintln(os.Stderr, "trellis-deploy:", err)
			return 2
		}
		f, err := os.OpenFile(*outFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			fmt.Fprintf(os.Stderr, "trellis-deploy: %v (an existing profile is never overwritten)\n", err)
			return 1
		}
		_, err = f.Write(y)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(*outFile) // a half-written profile would block the next try
			fmt.Fprintf(os.Stderr, "trellis-deploy: writing %s: %v\n", *outFile, err)
			return 1
		}
		fmt.Printf("Mesh %q written to %s. It holds the mesh's secret: keep it private.\nNext: trellis-deploy --profile %s NODES-FILE\n", *newMesh, *outFile, *outFile)
		return 0
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	fail := func(format string, args ...any) int {
		fmt.Fprintf(os.Stderr, "trellis-deploy: "+format+"\n", args...)
		return 1
	}

	// The nodes.
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return fail("%v", err)
	}
	hosts, err := deploy.ParseHosts(f)
	f.Close()
	if err != nil {
		return fail("%s: %v", fs.Arg(0), err)
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		return fail("needs the ssh client (OpenSSH)")
	}
	if *setup {
		ssh, err := deploy.NewSSH(sshOpts)
		if err != nil {
			return fail("%v", err)
		}
		defer ssh.Close()
		atExit = append(atExit, ssh.Close)
		return runSetup(ctx, ssh, hosts, *sshKey, *sudoNoPass, *yes, *dry)
	}

	// The release, checked before anything else.
	keys, err := release.Builtin()
	if err != nil {
		return fail("%v", err)
	}
	if *trust != "" {
		b, err := os.ReadFile(*trust)
		if err != nil {
			return fail("%v", err)
		}
		more, err := deb.ParsePublicKeys(string(b))
		if err != nil {
			return fail("%s: %v", *trust, err)
		}
		keys = append(keys, more...)
	}
	dir := *from
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return fail("%v", err)
		}
		fmt.Println("== Release")
		dir, err = deploy.Fetcher{Repo: *repo}.Fetch(ctx, *rel, filepath.Join(cache, "trellis-deploy"), os.Stdout)
		if err != nil {
			return fail("%v", err)
		}
	}
	r, err := deploy.LoadRelease(dir, keys, *from == "")
	if err != nil {
		return fail("the release did not check out, nothing was done: %v", err)
	}
	if *rel != "" && *from != "" && r.Version != strings.TrimPrefix(*rel, "v") {
		return fail("%s holds Trellis %s, not %s", dir, r.Version, *rel)
	}
	sums := "no SHA256SUMS"
	if r.Checksums {
		sums = "checksums match"
	}
	fmt.Printf("Trellis %s: %d package(s) signed by release key %s; %s\n", r.Version, len(r.Arches), r.SignedBy, sums)

	var profile []byte
	if *profilePath != "" {
		profile, err = os.ReadFile(*profilePath)
		if err != nil {
			return fail("%v", err)
		}
		p, err := config.ParseProfile(profile)
		if err != nil {
			return fail("%s is not a mesh profile this release can use: %v", *profilePath, err)
		}
		fmt.Printf("Mesh profile: %s (%s), for nodes that have none\n", p.Name, p.MeshID)
	}

	// The nodes, looked at.
	ssh, err := deploy.NewSSH(sshOpts)
	if err != nil {
		return fail("%v", err)
	}
	defer ssh.Close()
	atExit = append(atExit, ssh.Close)
	fmt.Printf("\n== Looking at %d node(s)\n", len(hosts))
	states := deploy.Inspect(ctx, ssh, hosts, r.Debian)
	steps := deploy.Plan(hosts, states, r, deploy.PlanOptions{Profile: len(profile) > 0, Force: *force, AnyVersion: *anyVersion})
	printPlan(steps)
	work := 0
	var asks []string
	for _, s := range steps {
		if s.Action == deploy.Install || s.Action == deploy.Upgrade {
			work++
			if !s.State.NoPass {
				asks = append(asks, s.Name())
			}
		}
	}
	if work == 0 {
		fmt.Println("\nNothing to do.")
		return 0
	}
	if len(asks) > 0 {
		fmt.Printf("\nsudo asks for a password on: %s\n", strings.Join(asks, ", "))
	}
	if *dry {
		fmt.Println("\nDry run: nothing was done.")
		return 0
	}
	if !*yes && !confirm(ctx, fmt.Sprintf("\nGo ahead with %d node(s)? [y/N] ", work)) {
		fmt.Println("Nothing was done.")
		return 1
	}

	runner := &deploy.Runner{SSH: ssh, Release: r, Profile: profile, Reboot: !*noReboot, Jobs: *jobs, KeepGoing: *keepGoing, Fast: *fast, Voice: *withVoice, Out: os.Stdout,
		DropNodeKey: *nodeKey == "" && *trust == ""}
	if *nodeKey != "" {
		b, err := os.ReadFile(*nodeKey)
		if err != nil {
			return fail("%v", err)
		}
		if ks, err := deb.ParsePublicKeys(string(b)); err != nil || len(ks) == 0 {
			return fail("%s is not a release public key", *nodeKey)
		}
		runner.NodeKey = b
	}
	if len(asks) > 0 {
		// Each password is tried on the other nodes too; a node none of
		// them opens is asked for by name. Kept only for this run.
		fmt.Println("\nsudo passwords (kept only for this run; Enter leaves a node out):")
		before := make([]deploy.Action, len(steps))
		for i, s := range steps {
			before[i] = s.Action
		}
		steps, err = runner.FindPasswords(ctx, steps, func(s deploy.Step, retry bool) (string, error) {
			if retry {
				fmt.Printf("  that did not work on %s\n", s.Name())
			}
			return secret(ctx, fmt.Sprintf("  sudo password on %s (%s): ", s.Name(), s.Host.Target))
		})
		if err != nil {
			return fail("%v", err)
		}
		left := 0
		for i, s := range steps {
			if before[i] != deploy.Skip && s.Action == deploy.Skip {
				fmt.Printf("  %s is left out: %s\n", s.Name(), s.Why)
				left++
			}
		}
		if left == work {
			fmt.Println("No node left to work on: nothing was done.")
			return 1
		}
	}
	installs := 0
	for _, s := range steps {
		if s.Action == deploy.Install {
			installs++
		}
	}
	// Setting it needs meshd running, which a new node does only after its
	// reboot: with --no-reboot it is set afterwards, on the node.
	if *webPass && !runner.Reboot {
		fmt.Println("\n--web-password does nothing with --no-reboot: a new node runs only after its reboot; set the password on it then (sudo meshctl web password).")
	}
	if installs > 0 && runner.Reboot && !*noWebPass && (*webPass || !*yes) {
		fmt.Println("\nThe web UI's admin password for the new nodes (at least 8 characters; Enter: set it later)")
		for {
			a, err := secret(ctx, "  password: ")
			if err != nil {
				return fail("%v", err)
			}
			if a == "" {
				break
			}
			b, _ := secret(ctx, "  again: ")
			if a == b && len(a) >= 8 {
				runner.WebPassword = a
				break
			}
			fmt.Println("  They differ, or it is shorter than 8 characters; again.")
		}
	}
	logDir := *logs
	if logDir == "" {
		logDir = filepath.Join("trellis-deploy-logs", time.Now().Format("20060102-150405"))
	}
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return fail("%v", err)
	}
	runner.Log = func(s deploy.Step) io.WriteCloser {
		name := s.Name()
		if !strings.Contains(name, s.Host.Key()) {
			name += "_" + s.Host.Key()
		}
		name = strings.NewReplacer("/", "_", ":", "_", "@", "_").Replace(name)
		f, err := os.OpenFile(filepath.Join(logDir, name+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nopCloser{io.Discard}
		}
		return f
	}

	fmt.Printf("\n== Deploying Trellis %s\n", r.Version)
	outs := runner.Run(ctx, steps)
	fmt.Println("\n== Summary")
	deploy.Summary(os.Stdout, outs)
	deploy.JoinDetails(os.Stdout, outs)
	installed := 0
	for _, o := range outs {
		if o.OK && o.Step.Action == deploy.Install {
			installed++
		}
	}
	if installed > 0 && runner.WebPassword == "" {
		fmt.Println("\nThe new nodes' web UI has no admin password yet: once a node runs, sudo meshctl web password on it.")
	}
	fmt.Printf("\nLogs: %s\n", logDir)
	if n := deploy.Failed(outs); n > 0 {
		fmt.Printf("%d node(s) did not finish: see their logs, and sudo meshctl doctor on the node.\n", n)
		return 1
	}
	return 0
}

// runSetup readies the nodes for later runs: the SSH key where it does not
// log in yet, and sudo without a password where it asks for one.
func runSetup(ctx context.Context, ssh *deploy.SSH, hosts []deploy.Host, key string, sudoNoPass, yes, dry bool) int {
	fmt.Printf("== Looking at %d node(s)\n", len(hosts))
	states := deploy.Inspect(ctx, ssh, hosts, "0")
	var needKey, needSudo, ready []int
	for i, st := range states {
		switch {
		case !st.Reachable && strings.Contains(st.Err, "Permission denied"):
			needKey = append(needKey, i)
		case !st.Reachable:
			fmt.Printf("  %-22s skipped: unreachable: %s\n", hosts[i].Key(), st.Err)
		case !st.NoPass && sudoNoPass:
			needSudo = append(needSudo, i)
		default:
			ready = append(ready, i)
		}
	}
	names := func(idx []int) string {
		var n []string
		for _, i := range idx {
			n = append(n, hosts[i].Target)
		}
		return strings.Join(n, ", ")
	}
	if len(ready) > 0 {
		fmt.Printf("  your key logs in already: %s\n", names(ready))
	}
	if len(needKey) > 0 {
		fmt.Printf("  your SSH key goes on: %s (each asks for its login password)\n", names(needKey))
	}
	if len(needSudo) > 0 {
		fmt.Printf("  sudo without a password for: %s\n", names(needSudo))
		fmt.Println("  (whoever logs in as that user with your key is then root on the device)")
	}
	if len(needKey)+len(needSudo) == 0 {
		fmt.Println("\nNothing to do.")
		return 0
	}
	if dry {
		fmt.Println("\nDry run: nothing was done.")
		return 0
	}
	if !yes && !confirm(ctx, "\nGo ahead? [y/N] ") {
		fmt.Println("Nothing was done.")
		return 1
	}
	rc := 0
	for _, i := range needKey {
		fmt.Printf("\n== %s: your SSH key\n", hosts[i].Target)
		if err := ssh.CopyKey(ctx, states[i].Route, key); err != nil {
			fmt.Printf("!! the key did not go on %s: %v\n", hosts[i].Target, err)
			rc = 1
		}
	}
	if !sudoNoPass {
		if rc == 0 {
			fmt.Println("\nDone: runs log in with your key. They ask for sudo passwords where a device needs one.")
		}
		return rc
	}
	// Look again: the nodes that took the key, and their sudo.
	if len(needKey) > 0 {
		states = deploy.Inspect(ctx, ssh, hosts, "0")
	}
	var steps []deploy.Step
	for i, st := range states {
		if st.Reachable && !st.NoPass {
			steps = append(steps, deploy.Step{Host: hosts[i], State: st, Action: deploy.Setup, Label: hosts[i].Target})
		}
	}
	if len(steps) == 0 {
		fmt.Println("\nEvery node reached logs in with your key and runs sudo without a password.")
		return rc
	}
	runner := &deploy.Runner{SSH: ssh, Out: os.Stdout}
	fmt.Println("\nsudo passwords (used once, to write the rule; Enter leaves a node out):")
	steps, err := runner.FindPasswords(ctx, steps, func(s deploy.Step, retry bool) (string, error) {
		if retry {
			fmt.Printf("  that did not work on %s\n", s.Name())
		}
		return secret(ctx, fmt.Sprintf("  sudo password on %s: ", s.Name()))
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "trellis-deploy:", err)
		return 1
	}
	fmt.Println()
	for _, s := range steps {
		if s.Action != deploy.Setup {
			fmt.Printf("  %-28s left out: %s\n", s.Name(), s.Why)
			rc = 1
			continue
		}
		var out strings.Builder
		if err := runner.GrantSudo(ctx, s, &out); err != nil {
			fmt.Printf("  %-28s !! sudo rule not written: %v %s\n", s.Name(), err, strings.TrimSpace(out.String()))
			rc = 1
			continue
		}
		fmt.Printf("  %-28s sudo without a password (%s)\n", s.Name(), deploy.SudoersFile)
	}
	return rc
}

func printPlan(steps []deploy.Step) {
	width := 4
	for _, s := range steps {
		width = max(width, len(s.Name()))
	}
	for _, s := range steps {
		where := s.Host.Key()
		if v := s.State.Route.Via; v != "" {
			through := v
			if _, host, ok := strings.Cut(v, "@"); ok {
				through = host
			}
			if p := s.State.Route.ViaPort; p != 0 {
				through += fmt.Sprintf(":%d", p)
			}
			where += " (mesh, via " + through + ")"
		}
		fmt.Printf("  %-*s  %-34s  %-6s  %-11s  %s\n", width, s.Name(), where, s.State.Arch, s.Action, s.Why)
	}
}

// atExit runs before an interrupt at a prompt ends the program (deferred
// calls do not run then): the shared ssh connections are closed.
var atExit []func()

// stdin is read through one buffer: two would each swallow lines.
var stdin = bufio.NewReader(os.Stdin)

// readLine reads a line from the terminal. The interrupt is caught for the
// run, so at a prompt Ctrl-C would do nothing: here it ends the program,
// with the terminal's echo back on.
func readLine(ctx context.Context) (string, error) {
	type answer struct {
		line string
		err  error
	}
	ch := make(chan answer, 1)
	go func() {
		l, err := stdin.ReadString('\n')
		ch <- answer{l, err}
	}()
	select {
	case a := <-ch:
		return a.line, a.err
	case <-ctx.Done():
		echo(true)
		fmt.Fprintln(os.Stderr, "\ninterrupted: stopped here")
		for _, f := range atExit {
			f()
		}
		os.Exit(130)
		return "", ctx.Err()
	}
}

func echo(on bool) bool {
	arg := "-echo"
	if on {
		arg = "echo"
	}
	c := exec.Command("stty", arg)
	c.Stdin = os.Stdin
	return c.Run() == nil
}

func confirm(ctx context.Context, q string) bool {
	fmt.Print(q)
	line, _ := readLine(ctx)
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

// secret reads a line without echoing it (stty, on Linux and macOS).
func secret(ctx context.Context, prompt string) (string, error) {
	fmt.Print(prompt)
	quiet := echo(false)
	line, err := readLine(ctx)
	if quiet {
		echo(true)
	}
	fmt.Println()
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }
