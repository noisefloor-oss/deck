// Command noisedeck-harness-bump moves the agent harness pins the image
// carries. It never merges anything: the scheduled workflow runs it,
// builds the image, and opens a pull request with the report.
//
//	noisedeck-harness-bump list
//	noisedeck-harness-bump model-plan [-root DIR] [-codex PATH] [-claude PATH] [-kimi PATH] [-files]
//	noisedeck-harness-bump check [-json] [-root DIR]
//	noisedeck-harness-bump batch [-root DIR]
//	noisedeck-harness-bump drop -harness NAME -reason TEXT -batch FILE [-root DIR]
//	noisedeck-harness-bump verify-batch -head SHA -id ID [-root DIR]
//	noisedeck-harness-bump apply -harness NAME [-version V] [-json] [-root DIR]
//	noisedeck-harness-bump probe -harness NAME -image IMAGE [-root DIR]
//	noisedeck-harness-bump settle -repo OWNER/NAME -root DIR [-pr N] [-bot LOGIN] [-apply] [-wait DURATION] [-json]
//	noisedeck-harness-bump broker -model NAME -listen unix:PATH|HOST:PORT [-upstream URL] [-port-file FILE]   (key from OPENAI_API_KEY)
//	noisedeck-harness-bump jail-dial -listen HOST:PORT -socket PATH
//	noisedeck-harness-bump verdict -capture FILE [-json]
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/noisefloor-oss/deck/internal/harnessbump"
	"github.com/noisefloor-oss/deck/internal/modelbump"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "noisedeck-harness-bump:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runContext(ctx, args, stdout, stderr)
}

func runContext(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: noisedeck-harness-bump list|check|apply|batch|drop|model-plan|verify-batch|probe|settle|broker|verdict|jail-dial")
	}
	switch args[0] {
	case "review-refs":
		fs := flag.NewFlagSet("review-refs", flag.ContinueOnError)
		repo := fs.String("repo", "", "owner/name")
		pr := fs.Int("pr", 0, "pull request number")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		snapshot, err := harnessbump.ReadReviewSnapshot(ctx, harnessbump.ExecGH, *repo, *pr)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(snapshot)
	case "list":
		for _, h := range harnessbump.Catalogue {
			_, _ = fmt.Fprintf(stdout, "%s\t%s\t%s\n", h.Name, h.VersionArg, h.Source)
		}
		return nil
	case "check":
		return check(ctx, args[1:], stdout)
	case "apply":
		return apply(ctx, args[1:], stdout, stderr)
	case "batch":
		fs := flag.NewFlagSet("batch", flag.ContinueOnError)
		root := fs.String("root", ".", "repository root")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		result, err := harnessbump.ApplyBatch(ctx, *root, harnessbump.NewResolver())
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(result)
	case "drop":
		return drop(args[1:], stdout)
	case "model-plan":
		fs := flag.NewFlagSet("model-plan", flag.ContinueOnError)
		root := fs.String("root", ".", "repository root")
		codex := fs.String("codex", "codex", "codex CLI executable for catalog discovery")
		claude := fs.String("claude", "claude", "claude CLI executable for membership probes")
		kimi := fs.String("kimi", "kimi", "kimi CLI executable for catalog discovery")
		files := fs.Bool("files", false, "include the planned file contents in the JSON output")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		truth, err := modelbump.ReadTruth(*root)
		if err != nil {
			return err
		}
		plan, err := modelbump.Discover(ctx, truth, *codex, *claude, *kimi, modelbump.CodexCatalog, modelbump.ClaudeMember, modelbump.KimiCatalog)
		if err != nil {
			return err
		}
		out := struct {
			modelbump.Plan
			Files   map[string][]byte `json:"files,omitempty"`
			Sources map[string]string `json:"sources,omitempty"`
		}{Plan: plan}
		if *files {
			planFiles, err := modelbump.Files(*root, plan.Offers)
			if err != nil {
				return err
			}
			out.Files = map[string][]byte{}
			out.Sources = map[string]string{}
			for path, planned := range planFiles {
				out.Files[path] = planned.Body
				out.Sources[path] = planned.SourceSHA
			}
		}
		return json.NewEncoder(stdout).Encode(out)
	case "verify-batch":
		fs := flag.NewFlagSet("verify-batch", flag.ContinueOnError)
		root := fs.String("root", ".", "trusted repository root")
		head := fs.String("head", "", "candidate commit")
		id := fs.String("id", "", "batch identity")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if len(*head) != 40 || len(*id) != 64 {
			return errors.New("head and batch id are required")
		}
		ok, err := harnessbump.Reconstruct(*root, harnessbump.NewResolver())(ctx, harnessbump.Harness{Name: "all"}, *id, *head)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("candidate is not the complete deterministic batch")
		}
		return nil
	case "probe":
		return probe(ctx, args[1:], stdout)
	case "settle":
		return settle(ctx, args[1:], stdout)
	case "broker":
		return broker(ctx, args[1:], stdout)
	case "review-proxy":
		return reviewProxy(ctx, args[1:])
	case "review-prompt":
		fs := flag.NewFlagSet("review-prompt", flag.ContinueOnError)
		diffPath := fs.String("diff", "", "immutable base/head compare diff")
		repo := fs.String("repo", "", "trusted repository")
		pr := fs.Int64("pr", 0, "trusted pull request")
		head := fs.String("head", "", "immutable head commit")
		base := fs.String("base", "", "immutable base commit")
		evidencePath := fs.String("evidence", "", "optional bounded JSON evidence")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		diff, err := os.ReadFile(*diffPath)
		if err != nil {
			return err
		}
		prompt, err := io.ReadAll(io.LimitReader(os.Stdin, (2<<20)+1))
		if err != nil {
			return err
		}
		var evidence []byte
		if *evidencePath != "" {
			f, err := os.Open(*evidencePath)
			if err != nil {
				return err
			}
			evidence, err = io.ReadAll(io.LimitReader(f, harnessbump.MaxReviewEvidenceBytes+1))
			_ = f.Close()
			if err != nil {
				return err
			}
		}
		bound, err := harnessbump.BoundReviewPrompt(prompt, diff, evidence, harnessbump.ReviewSnapshot{Repository: *repo, PullRequest: *pr, Head: *head, Base: *base})
		if err != nil {
			return err
		}
		_, err = stdout.Write(bound)
		return err
	case "verdict":
		return verdict(args[1:], stdout)
	case "jail-dial":
		return jailDial(ctx, args[1:], stdout)
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func reviewProxy(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("review-proxy", flag.ContinueOnError)
	socket := fs.String("socket", "", "private unix socket")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *socket == "" {
		return errors.New("-socket is required")
	}
	ln, err := net.Listen("unix", *socket)
	if err != nil {
		return err
	}
	defer ln.Close() //nolint:errcheck // Server shutdown also closes the listener.
	if err := os.Chmod(*socket, 0o600); err != nil {
		return err
	}
	srv := &http.Server{Handler: harnessbump.ReviewProxy{}, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// drop returns one harness to its pre-batch pin after its gates failed and
// rewrites the batch report, so the rest of the batch still ships.
func drop(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("drop", flag.ContinueOnError)
	root := fs.String("root", ".", "repository root")
	name := fs.String("harness", "", "harness to return to its pre-batch pin")
	reason := fs.String("reason", "", "why the harness leaves the batch")
	batchFile := fs.String("batch", "", "batch report to update in place")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" || *reason == "" || *batchFile == "" {
		return errors.New("drop requires -harness, -reason and -batch")
	}
	raw, err := os.ReadFile(*batchFile)
	if err != nil {
		return err
	}
	var result harnessbump.BatchResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("batch report %s: %w", *batchFile, err)
	}
	if err := harnessbump.Drop(*root, &result, *name, *reason); err != nil {
		return err
	}
	out, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(*batchFile, append(out, '\n'), 0o600); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "dropped %s; batch %s ships %d update(s)\n", *name, result.ID, len(result.Updated))
	return err
}

func check(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	root := fs.String("root", ".", "repository root")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	statuses, err := harnessbump.Check(ctx, *root, harnessbump.NewResolver(), harnessbump.Catalogue)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(stdout).Encode(statuses)
	}
	for _, s := range statuses {
		switch {
		case s.Note != "":
			_, _ = fmt.Fprintf(stdout, "%-13s %-20s %s\n", s.Harness, s.Pinned, s.Note)
		case s.Outdated:
			_, _ = fmt.Fprintf(stdout, "%-13s %-20s -> %s\n", s.Harness, s.Pinned, s.Latest)
		default:
			_, _ = fmt.Fprintf(stdout, "%-13s %-20s current\n", s.Harness, s.Pinned)
		}
	}
	return nil
}

// applyResult is what the workflow reads back from an apply.
type applyResult struct {
	Harness string            `json:"harness"`
	From    string            `json:"from"`
	To      string            `json:"to"`
	Changed []string          `json:"changed"`
	Digests map[string]string `json:"digests,omitempty"`
}

func apply(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	root := fs.String("root", ".", "repository root")
	name := fs.String("harness", "", "harness to move")
	version := fs.String("version", "", "version to pin (default: upstream latest)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	h, err := harnessbump.Lookup(*name)
	if err != nil {
		return err
	}
	resolver := harnessbump.NewResolver()
	from, _, err := harnessbump.Pinned(*root, h)
	if err != nil {
		return err
	}
	to := *version
	if to == "" {
		to, err = resolver.Latest(ctx, h)
		if err != nil {
			return err
		}
	}
	if !harnessbump.ValidVersion(to) {
		return fmt.Errorf("%q is not a version", to)
	}
	result := applyResult{Harness: h.Name, From: from, To: to}
	if from != to {
		if len(h.Artifacts) > 0 {
			_, _ = fmt.Fprintf(stderr, "measuring %d artifact digest(s) for %s %s\n", len(h.Artifacts), h.Name, to)
		}
		digests, err := resolver.Digests(ctx, h, to)
		if err != nil {
			return err
		}
		result.Digests = digests
		result.Changed, err = harnessbump.Apply(*root, harnessbump.Bump{Harness: h, From: from, To: to, Digests: digests})
		if err != nil {
			return err
		}
	}
	if *asJSON {
		return json.NewEncoder(stdout).Encode(result)
	}
	if from == to {
		_, _ = fmt.Fprintf(stdout, "%s is already %s\n", h.Name, to)
		return nil
	}
	_, _ = fmt.Fprintf(stdout, "%s %s -> %s\n", h.Name, from, to)
	for _, f := range result.Changed {
		_, _ = fmt.Fprintf(stdout, "  %s\n", f)
	}
	return nil
}

func probe(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	root := fs.String("root", ".", "repository root")
	name := fs.String("harness", "", "harness to probe")
	image := fs.String("image", "", "built image to probe")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *image == "" {
		return errors.New("-image is required")
	}
	h, err := harnessbump.Lookup(*name)
	if err != nil {
		return err
	}
	version, _, err := harnessbump.Pinned(*root, h)
	if err != nil {
		return err
	}
	spec, err := harnessbump.LoadProbe(*root, h.Name)
	if err != nil {
		return err
	}
	receive, err := harnessbump.LoadReceiveProbe(*root, h.Name)
	if err != nil {
		return err
	}
	network := ""
	if receive != nil {
		network = receive.Network
	}
	report, probeErr := harnessbump.Probe(ctx, spec, version, harnessbump.DockerRunner(*image, h.Executable))
	for _, line := range report {
		_, _ = fmt.Fprintln(stdout, line)
	}
	receiveReport, receiveErr := harnessbump.ProbeReceive(ctx, receive, harnessbump.ReceiveDockerRunner(*image, network))
	for _, line := range receiveReport {
		_, _ = fmt.Fprintln(stdout, line)
	}
	return errors.Join(probeErr, receiveErr)
}

// settle merges the job's pull requests that the reviewer approved at the
// head with the gate green, brings an approved one up to date when main
// moved, and names the ones that still need a review.
func settle(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("settle", flag.ContinueOnError)
	repo := fs.String("repo", "", "owner/name")
	root := fs.String("root", ".", "repository root, checked out at main, from which each bump is reconstructed")
	only := fs.Int("pr", 0, "settle one pull request (default: every job branch)")
	bot := fs.String("bot", "noise-review[bot]", "the reviewer whose verdict counts")
	apply := fs.Bool("apply", false, "perform the merge or branch update (default: decide only)")
	wait := fs.Duration("wait", 0, "wait up to this duration for CI or branch updates before returning a review decision")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *repo == "" {
		return errors.New("-repo is required")
	}
	rootDir, err := harnessbump.ReconstructRoot(*root)
	if err != nil {
		return err
	}
	if *wait < 0 || (*wait > 0 && !*apply) {
		return errors.New("-wait requires -apply and a nonnegative duration")
	}
	settleOnce := func(ctx context.Context) ([]harnessbump.Decision, error) {
		return harnessbump.Settle(ctx, harnessbump.ExecGH, *repo, *bot, *only, *apply, harnessbump.Reconstruct(rootDir, harnessbump.NewResolver()), harnessbump.WithMergeLift(rootDir, harnessbump.RunEvidence(*repo)))
	}
	var decisions []harnessbump.Decision
	if *wait > 0 {
		waitCtx, cancel := context.WithTimeout(ctx, *wait)
		defer cancel()
		decisions, err = harnessbump.WaitForSettlement(waitCtx, 20*time.Second, settleOnce)
	} else {
		decisions, err = settleOnce(ctx)
	}
	if *asJSON {
		if decisions == nil {
			decisions = []harnessbump.Decision{}
		}
		return errors.Join(err, json.NewEncoder(stdout).Encode(decisions))
	}
	if err != nil {
		return err
	}
	if len(decisions) == 0 {
		_, _ = fmt.Fprintln(stdout, "no open pull request on a harness-bump/ branch")
	}
	for _, d := range decisions {
		did := ""
		if d.Applied {
			did = " (done)"
		}
		_, _ = fmt.Fprintf(stdout, "#%d %s %s%s: %s\n", d.Number, d.Branch, d.Action, did, d.Reason)
	}
	return nil
}

// broker serves the reviewer jail's model calls with the real key from
// OPENAI_API_KEY, which never leaves this process. It prints the bound
// address on the first line and serves until interrupted.
func broker(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("broker", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:0", "address to listen on")
	upstream := fs.String("upstream", "https://api.openai.com", "the model API")
	portFile := fs.String("port-file", "", "write the bound port here once listening")
	model := fs.String("model", "", "the one model the round was dispatched with")
	statsFile := fs.String("stats-file", "", "record admitted, refused and forwarded counts here after every request")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *model == "" {
		return errors.New("-model is required: the broker admits one model")
	}
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return errors.New("OPENAI_API_KEY is not set")
	}
	u, err := url.Parse(*upstream)
	if err != nil || u.Scheme != "https" {
		return fmt.Errorf("-upstream must be an https URL: %q", *upstream)
	}
	var ln net.Listener
	if path, ok := strings.CutPrefix(*listen, "unix:"); ok {
		_ = os.Remove(path)
		ln, err = net.Listen("unix", path)
		if err != nil {
			return err
		}
		// The jail connects as the same user; nobody else needs to.
		if err := os.Chmod(path, 0o600); err != nil {
			return err
		}
	} else {
		ln, err = net.Listen("tcp", *listen)
		if err != nil {
			return err
		}
	}
	_, _ = fmt.Fprintln(stdout, ln.Addr().String())
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok && *portFile != "" {
		if err := os.WriteFile(*portFile, []byte(fmt.Sprintf("%d\n", tcp.Port)), 0o600); err != nil {
			return err
		}
	}
	srv := &http.Server{Handler: harnessbump.NewBrokerWithStats(u, key, harnessbump.DefaultBrokerPolicy(*model), *statsFile), ReadHeaderTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// verdict validates what the jail captured before the host posts it.
func verdict(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("verdict", flag.ContinueOnError)
	capture := fs.String("capture", "", "the jail's capture: the event on the first line, the body after it")
	asJSON := fs.Bool("json", false, "machine-readable output")
	head := fs.String("head", "", "require verdict binding to this head")
	base := fs.String("base", "", "require verdict binding to this base")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *capture == "" {
		return errors.New("-capture is required")
	}
	raw, err := harnessbump.ReadCapture(*capture)
	if err != nil {
		return err
	}
	v, err := harnessbump.ParseVerdict(raw)
	if err != nil {
		return err
	}
	if *head != "" || *base != "" {
		if err := harnessbump.ValidateReviewBinding(v, *head, *base); err != nil {
			return err
		}
	}
	if *asJSON {
		return json.NewEncoder(stdout).Encode(v)
	}
	_, _ = fmt.Fprintf(stdout, "%s\n%s\n", v.Event, v.Body)
	return nil
}

// jailDial runs inside the jail: the loopback listener that forwards to
// the broker's bind-mounted unix socket, the jail's only way out.
func jailDial(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("jail-dial", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:8080", "loopback address to listen on")
	socket := fs.String("socket", "", "the broker's unix socket")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *socket == "" {
		return errors.New("-socket is required")
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stdout, ln.Addr().String())
	if err := harnessbump.JailDial(ctx, ln, *socket); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
