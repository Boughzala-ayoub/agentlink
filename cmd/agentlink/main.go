// agentlink lets the AI agents on one machine message each other.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentlink/mcp"
	"agentlink/setup"
	"agentlink/store"
	"agentlink/wake"
)

const usage = `agentlink: messaging between the AI agents on this machine.

  agentlink setup [--yes] [--show] [--only app,app]   connect the installed apps
  agentlink doctor                    check every app and agentlink itself
  agentlink uninstall [--yes]         take agentlink out of every app's config
  agentlink peers                     list the agents agentlink knows
  agentlink watch                     print every message as it is sent
  agentlink send [flags] <to> <text>  send a message as "cli"
  agentlink resume <thread>           let a thread the loop guard paused go on
  agentlink mcp --host <app>          stdio MCP server; each app starts this itself

Apps: claude-code, claude-desktop, codex, opencode, cursor, vscode, gemini, devin.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, rest := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "setup":
		err = runSetup(rest)
	case "doctor":
		if !setup.Doctor(setup.RealEnv(), installedExe(), os.Stdout) {
			os.Exit(1)
		}
	case "uninstall":
		err = runUninstall(rest)
	case "hook": // agentlink hook <app> <event>, run by an app's hooks
		var st *store.Store
		if len(rest) == 2 {
			if st, err = open(); err == nil {
				err = wake.Hook(st, rest[0], rest[1], os.Stdin, os.Stdout)
				st.Close()
			}
		}
		if err != nil { // never block the app's turn over agentlink
			fmt.Fprintln(os.Stderr, "agentlink hook:", err)
			err = nil
		}
	case "opencode-feed": // started by agentlink's opencode plugin
		var st *store.Store
		if st, err = open(); err == nil {
			err = wake.OpencodeFeed(st, os.Stdin, os.Stdout)
			st.Close()
		}
	case "mcp":
		err = runMCP(rest)
	case "send":
		err = runSend(rest)
	case "resume":
		err = runResume(rest)
	case "peers":
		err = runPeers()
	case "watch":
		err = runWatch()
	case "version", "--version":
		fmt.Println(mcp.Version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "agentlink: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentlink:", err)
		os.Exit(1)
	}
}

func open() (*store.Store, error) { return store.Open("") }

func installedExe() string { return filepath.Join(setup.RealEnv().AgentHome, "bin", setup.ExeName()) }

func confirm(question string) bool {
	fmt.Print(question + " [Y/n] ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		fmt.Println("\nNo answer (stdin is closed). Run again with --yes to skip the question.")
		return false
	}
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "" || a == "y" || a == "yes"
}

func runSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	yes := fs.Bool("yes", false, "don't ask")
	show := fs.Bool("show", false, "print every change before asking")
	only := fs.String("only", "", "comma-separated apps to set up, e.g. claude-code,codex")
	fs.Parse(args)
	var onlyList []string
	if *only != "" {
		onlyList = strings.Split(*only, ",")
	}
	e := setup.RealEnv()
	exe := installedExe()
	changes, found, err := setup.Plan(e, exe, onlyList)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		fmt.Println("agentlink found none of the apps it supports:")
		for _, a := range setup.Apps {
			fmt.Println("  " + a.Name)
		}
		return nil
	}
	if len(changes) == 0 {
		fmt.Println("agentlink is already set up for every app it found.")
		if _, err := setup.InstallSelf(e); err != nil {
			return err
		}
		fmt.Println()
		setup.Doctor(e, exe, os.Stdout)
		return nil
	}
	fmt.Printf("agentlink setup will install itself to %s and change these files:\n\n", exe)
	for _, c := range changes {
		fmt.Printf("  %-15s %s: %s\n", c.Name, setup.Tilde(e, c.Path), c.What)
		if *show {
			printDiff(c.Old, c.New)
		}
	}
	fmt.Println("\nEach file is backed up next to itself first. `agentlink uninstall` undoes all of it.")
	if !*yes && !confirm("Go ahead?") {
		fmt.Println("Nothing changed.")
		return nil
	}
	if !claudeDesktopClosed(changes, *yes) {
		var keep []setup.Change
		for _, c := range changes {
			if c.App != "claude-desktop" {
				keep = append(keep, c)
			}
		}
		changes = keep
		fmt.Println("Leaving Claude desktop out. Run `agentlink setup` again once it is closed.")
	}
	if _, err := setup.InstallSelf(e); err != nil {
		return err
	}
	if err := setup.Apply(e, changes, time.Now()); err != nil {
		return err
	}
	// A running app may rewrite its config at any moment; check our entries survived.
	if again, _, err := setup.Plan(e, exe, onlyList); err == nil && len(again) > 0 {
		for _, c := range again {
			fmt.Printf("\n%s rewrote %s right after setup changed it. Quit %s and run `agentlink setup` again.\n", c.Name, setup.Tilde(e, c.Path), c.Name)
		}
	}
	fmt.Println("\nDone. Restart these apps so they load agentlink. Setup doesn't restart or close anything itself:")
	seen := map[string]bool{}
	for _, c := range changes {
		for _, a := range setup.Apps {
			if a.Host == c.App && !seen[a.Host] {
				seen[a.Host] = true
				fmt.Println("  - " + a.Restart)
			}
		}
	}
	fmt.Println()
	setup.Doctor(e, exe, os.Stdout)
	return nil
}

// claudeDesktopClosed makes sure Claude desktop isn't running before its config
// changes, since it writes its own copy back when it quits. It never closes the
// app: it asks the user to, and reports false if they skip.
func claudeDesktopClosed(changes []setup.Change, yes bool) bool {
	touches := false
	for _, c := range changes {
		touches = touches || c.App == "claude-desktop"
	}
	if !touches || !setup.ClaudeDesktopRunning() {
		return true
	}
	fmt.Println("\nClaude desktop is running. It saves its own copy of claude_desktop_config.json when it quits,")
	fmt.Println("which would drop agentlink's entry, so its file can only change while it is closed.")
	if yes {
		return false
	}
	in := bufio.NewReader(os.Stdin)
	for setup.ClaudeDesktopRunning() {
		fmt.Print("Quit Claude desktop (right-click its tray icon, Quit), then press Enter. Type skip to leave it out: ")
		line, err := in.ReadString('\n')
		if err != nil || strings.EqualFold(strings.TrimSpace(line), "skip") {
			return false
		}
	}
	return true
}

// printDiff shows the lines between the common start and end of two files.
func printDiff(a, b []byte) {
	x, y := strings.Split(string(a), "\n"), strings.Split(string(b), "\n")
	i := 0
	for i < len(x) && i < len(y) && x[i] == y[i] {
		i++
	}
	j := 0
	for j < len(x)-i && j < len(y)-i && x[len(x)-1-j] == y[len(y)-1-j] {
		j++
	}
	for _, l := range x[i : len(x)-j] {
		fmt.Println("      - " + strings.TrimRight(l, "\r"))
	}
	for _, l := range y[i : len(y)-j] {
		fmt.Println("      + " + strings.TrimRight(l, "\r"))
	}
}

func runUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	yes := fs.Bool("yes", false, "don't ask")
	fs.Parse(args)
	if !*yes && !confirm("Take agentlink out of every app's config?") {
		fmt.Println("Nothing changed.")
		return nil
	}
	e := setup.RealEnv()
	if setup.ClaudeDesktopRunning() {
		fmt.Println("Claude desktop is running and would put agentlink's entry back when it quits.")
		fmt.Print("Quit it (right-click its tray icon, Quit), then press Enter: ")
		bufio.NewReader(os.Stdin).ReadString('\n')
	}
	done, err := setup.Uninstall(e)
	for _, p := range done {
		fmt.Println("  " + p)
	}
	if err != nil {
		return err
	}
	if len(done) == 0 {
		fmt.Println("No app config mentions agentlink.")
	}
	fmt.Printf("\nRestart the apps above to unload agentlink. The backups stay next to each file.\n%s still holds the database and the executable; delete that folder to remove everything.\n", e.AgentHome)
	return nil
}

func runMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	host := fs.String("host", "", "the app that runs this server")
	fs.Parse(args)
	st, err := open()
	if err != nil {
		return err
	}
	defer st.Close()
	return (&mcp.Server{Host: *host, Store: st}).Serve(os.Stdin, os.Stdout)
}

var cli = store.Endpoint{Handle: "cli", App: "cli", Tier: "pull"}

func runSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	thread := fs.String("thread", "", "thread to continue")
	replyTo := fs.String("reply-to", "", "id of the message this answers")
	fs.Parse(args)
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: agentlink send [--thread T] [--reply-to ID] <to> <text>")
	}
	st, err := open()
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Register(cli); err != nil {
		return err
	}
	if *thread != "" { // a person writing into a thread counts as a person typing in it
		if _, err := st.Resume(*thread); err != nil {
			return err
		}
	}
	wake.Discover(st, fs.Arg(0))
	rcpt, err := st.Resolve(cli, fs.Arg(0))
	if err != nil {
		return err
	}
	var to []string
	for _, r := range rcpt {
		to = append(to, r.Handle)
	}
	sent, err := st.Send(cli, to, strings.Join(fs.Args()[1:], " "), *thread, *replyTo)
	if err != nil {
		return err
	}
	if sent.Duplicate {
		fmt.Printf("duplicate of %s, not sent again (thread %s)\n", sent.ID, sent.Thread)
		return nil
	}
	fmt.Printf("sent %s in thread %s\n", sent.ID, sent.Thread)
	for _, r := range rcpt {
		fmt.Printf("  %s: %s\n", r.Handle, wake.Deliver(st, r, sent))
	}
	return nil
}

// runResume restarts a thread's loop guards and wakes whoever still has unread messages in it.
func runResume(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: agentlink resume <thread>")
	}
	st, err := open()
	if err != nil {
		return err
	}
	defer st.Close()
	rs, err := st.Resume(args[0])
	if err != nil {
		return err
	}
	fmt.Printf("thread %s can go on\n", args[0])
	for _, r := range rs {
		fmt.Printf("  %s: %s\n", r.Handle, wake.Deliver(st, r, store.Sent{Thread: args[0]}))
	}
	return nil
}

func runPeers() error {
	st, err := open()
	if err != nil {
		return err
	}
	defer st.Close()
	ps, err := st.Peers()
	if err != nil {
		return err
	}
	if len(ps) == 0 {
		fmt.Println("no agents yet: an agent appears once its app starts agentlink")
	}
	for _, p := range ps {
		fmt.Printf("%-32s %-15s %-14s %-5s seen %-8s ago  %d unread\n", p.Handle, p.App, p.Project, p.Tier, store.Ago(p.LastSeen), p.Unread)
	}
	return nil
}

func runWatch() error {
	st, err := open()
	if err != nil {
		return err
	}
	defer st.Close()
	var last int64
	msgs, err := st.After(0)
	if err != nil {
		return err
	}
	if len(msgs) > 10 { // start with recent context, not the whole history
		msgs = msgs[len(msgs)-10:]
	}
	for {
		for _, m := range msgs {
			body := strings.ReplaceAll(strings.TrimRight(m.Body, "\n"), "\n", "\n    ")
			fmt.Printf("%s  %s -> %s  [%s]\n    %s\n", time.UnixMilli(m.CreatedAt).Format("15:04:05"), m.From, strings.Join(m.To, ", "), m.Thread, body)
			last = m.Seq
		}
		time.Sleep(250 * time.Millisecond)
		if msgs, err = st.After(last); err != nil {
			return err
		}
	}
}
