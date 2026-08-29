// verdict — the loadgen test rig CLI.
//
//	verdict list --scenarios ./scenarios
//	verdict run  --scenarios ./scenarios --id payment-db-slow-ramp
//	verdict suite --scenarios ./scenarios [--tags detection,rca]
//	verdict reset          # kill-switch every target
//
// Environment:
//
//	INFRASAGE_BASE_URL  (default http://localhost:8081)
//	INFRASAGE_TOKEN     bearer token for an operator account
//	INFRASAGE_TENANT    optional X-Tenant-ID
//	LOADGEN_HOST        default base for targets (default http://localhost)
//
// Targets default to the docker-compose ports (gateway 8080 … notification
// 8085); override any with --targets "payment=http://10.0.0.4:8084,...".
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/loadgen/internal/harness"
)

var defaultPorts = map[string]int{
	"gateway": 8080, "auth": 8081, "user": 8082,
	"order": 8083, "payment": 8084, "notification": 8085, "shopify": 8090,
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if len(os.Args) < 2 {
		usageExit()
	}
	cmd := os.Args[1]

	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	scenarioDir := fs.String("scenarios", "./scenarios", "scenario yaml directory")
	id := fs.String("id", "", "single scenario id (run)")
	tags := fs.String("tags", "", "comma-separated tag filter (suite)")
	targetsFlag := fs.String("targets", "", "override targets: name=url,name=url")
	reportDir := fs.String("report-dir", "./reports", "verdict output directory")
	skipRemediation := fs.Bool("skip-remediation", false, "skip remediation phases")
	_ = fs.Parse(os.Args[2:])

	targets := buildTargets(*targetsFlag)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "list":
		scenarios := mustLoad(*scenarioDir)
		for _, s := range scenarios {
			fmt.Printf("%-34s %-40s tags=%v\n", s.ID, s.Title, s.Tags)
		}

	case "reset":
		client := &http.Client{Timeout: 10 * time.Second}
		for name, base := range targets {
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chaos/kill-switch", nil)
			if resp, err := client.Do(req); err == nil {
				resp.Body.Close()
				fmt.Printf("reset %-14s ok\n", name)
			} else {
				fmt.Printf("reset %-14s FAILED: %v\n", name, err)
			}
		}

	case "run", "suite":
		scenarios := mustLoad(*scenarioDir)
		if cmd == "run" {
			if *id == "" {
				fatal("run needs --id")
			}
			scenarios = filterByID(scenarios, *id)
			if len(scenarios) == 0 {
				fatal("no scenario with id " + *id)
			}
		} else if *tags != "" {
			scenarios = filterByTags(scenarios, strings.Split(*tags, ","))
		}
		infra := infraFromEnv()
		runner := harness.NewRunner(infra, targets)
		runner.SkipRemediation = *skipRemediation

		started := time.Now().UTC()
		var runs []harness.RunResult
		for i, s := range scenarios {
			slog.Info("scenario starting", "id", s.ID, "n", fmt.Sprintf("%d/%d", i+1, len(scenarios)))
			runs = append(runs, runner.Run(ctx, s))
			if ctx.Err() != nil {
				break
			}
		}
		rep := harness.BuildSuiteReport(infra.BaseURL, started, runs)
		mdPath, err := rep.Write(*reportDir)
		if err != nil {
			fatal("write report: " + err.Error())
		}
		fmt.Println(rep.Markdown())
		fmt.Printf("report: %s\n", mdPath)
		if rep.Failed > 0 {
			os.Exit(1)
		}

	default:
		usageExit()
	}
}

func infraFromEnv() *harness.InfraSage {
	base := envOr("INFRASAGE_BASE_URL", "http://localhost:8081")
	token := os.Getenv("INFRASAGE_TOKEN")
	if token == "" {
		slog.Warn("INFRASAGE_TOKEN unset — authenticated endpoints will 401")
	}
	return harness.NewInfraSage(base, token, os.Getenv("INFRASAGE_TENANT"))
}

func buildTargets(override string) map[string]string {
	host := strings.TrimRight(envOr("LOADGEN_HOST", "http://localhost"), "/")
	out := map[string]string{}
	for name, port := range defaultPorts {
		out[name] = fmt.Sprintf("%s:%d", host, port)
	}
	for _, kv := range strings.Split(override, ",") {
		if kv == "" {
			continue
		}
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			out[strings.TrimSpace(parts[0])] = strings.TrimRight(strings.TrimSpace(parts[1]), "/")
		}
	}
	return out
}

func mustLoad(dir string) []harness.Scenario {
	scenarios, err := harness.LoadDir(dir)
	if err != nil {
		fatal(err.Error())
	}
	if len(scenarios) == 0 {
		fatal("no scenarios in " + dir)
	}
	return scenarios
}

func filterByID(in []harness.Scenario, id string) []harness.Scenario {
	var out []harness.Scenario
	for _, s := range in {
		if s.ID == id {
			out = append(out, s)
		}
	}
	return out
}

func filterByTags(in []harness.Scenario, tags []string) []harness.Scenario {
	want := map[string]bool{}
	for _, t := range tags {
		want[strings.TrimSpace(strings.ToLower(t))] = true
	}
	var out []harness.Scenario
	for _, s := range in {
		for _, t := range s.Tags {
			if want[strings.ToLower(t)] {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "verdict: "+msg)
	os.Exit(2)
}

func usageExit() {
	fmt.Fprintln(os.Stderr, `usage: verdict <list|run|suite|reset> [flags]
  list   — show scenarios
  run    — one scenario:  verdict run --id <scenario-id>
  suite  — all (or --tags filtered) scenarios, paced, with a scorecard
  reset  — kill-switch every chaos fault on every target`)
	os.Exit(2)
}
