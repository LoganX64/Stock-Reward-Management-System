// Command phase0 runs the entire Stocky Phase 0 baseline without PowerShell.
//
// It builds the API, dbtool, and load generator; creates and seeds the
// benchmark database; starts the API against that database; captures query
// plans; runs the load phases; captures database statistics; and stops the API.
//
// Phase 0 only. It does not create projection tables or change handlers.
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type options struct {
	database       string
	dbHost         string
	dbPort         string
	dbUser         string
	port           int
	users          int
	quick          bool
	skipLoad       bool
	skipPlans      bool
	skipSeed       bool
	onlyPeak       bool
	explainTimeout time.Duration
	outputRoot     string
}

func main() {
	var opts options
	flag.StringVar(&opts.database, "database", "stocky_bench", "benchmark database")
	flag.StringVar(&opts.dbHost, "db-host", "", "override database host")
	flag.StringVar(&opts.dbPort, "db-port", "", "override database port")
	flag.StringVar(&opts.dbUser, "db-user", "", "override database user")
	flag.IntVar(&opts.port, "port", 18080, "API port")
	flag.IntVar(&opts.users, "users", 20000, "number of seeded users")
	flag.BoolVar(&opts.quick, "quick", false, "short smoke run: 30s warm-up, 2m steady, 1m peak")
	flag.BoolVar(&opts.skipLoad, "skip-load", false, "seed and capture plans without running the load test")
	flag.BoolVar(&opts.skipPlans, "skip-plans", false, "skip EXPLAIN capture and go straight to the load test")
	flag.BoolVar(&opts.skipSeed, "skip-seed", false, "reuse the existing benchmark data instead of reseeding")
	flag.BoolVar(&opts.onlyPeak, "only-peak", false, "run only the 1,000 req/s peak phase for 1 minute")
	flag.DurationVar(&opts.explainTimeout, "explain-timeout", 10*time.Minute, "timeout for each EXPLAIN statement")
	flag.StringVar(&opts.outputRoot, "output-root", "bench/results", "results directory")
	flag.Parse()

	if err := run(opts); err != nil {
		fmt.Fprintf(os.Stderr, "phase0: %v\n", err)
		os.Exit(1)
	}
}

func run(opts options) error {
	repoRoot, err := os.Getwd()
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err != nil {
		return fmt.Errorf("run from the repository root: %w", err)
	}

	binDir := filepath.Join(repoRoot, "bench", "bin")
	resultDir := filepath.Join(repoRoot, opts.outputRoot, "phase0-"+time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(resultDir, 0o755); err != nil {
		return err
	}

	apiExe := filepath.Join(binDir, exeName("stocky-api"))
	dbtoolExe := filepath.Join(binDir, exeName("dbtool"))
	loadgenExe := filepath.Join(binDir, exeName("loadgen"))
	rebuildExe := filepath.Join(binDir, exeName("rebuild-projections"))

	fmt.Printf("result directory: %s\n", resultDir)

	if err := build(repoRoot, apiExe, "./cmd/stocky-api"); err != nil {
		return err
	}
	if err := build(repoRoot, dbtoolExe, "./bench/dbtool"); err != nil {
		return err
	}
	if err := build(repoRoot, loadgenExe, "./bench/loadgen"); err != nil {
		return err
	}
	if err := build(repoRoot, rebuildExe, "./cmd/rebuild-projections"); err != nil {
		return err
	}

	baseEnv := withEnv(os.Environ(), map[string]string{
		"POSTGRES_DB": opts.database,
	})

	// -----------------------------------------------------------------------
	// Database setup
	// -----------------------------------------------------------------------

	fmt.Println("setting up benchmark database...")
	setupArgs := dbtoolArgs(opts, "setup")
	if !opts.skipSeed {
		// A clean database is required so the API applies every migration before
		// any data exists. 0027 refuses to switch views over empty projections.
		setupArgs = dbtoolArgs(opts, "setup", "-recreate")
	}
	if err := runCommand(dbtoolExe, setupArgs, repoRoot, baseEnv, os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("database setup: %w", err)
	}

	// -----------------------------------------------------------------------
	// API process
	// -----------------------------------------------------------------------

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", opts.port)
	apiOutPath := filepath.Join(resultDir, "api.out.log")
	apiErrPath := filepath.Join(resultDir, "api.err.log")
	apiOut, err := os.Create(apiOutPath)
	if err != nil {
		return err
	}
	defer apiOut.Close()
	apiErr, err := os.Create(apiErrPath)
	if err != nil {
		return err
	}
	defer apiErr.Close()

	apiEnv := withEnv(os.Environ(), map[string]string{
		"POSTGRES_DB":                  opts.database,
		"HTTP_PORT":                    strconv.Itoa(opts.port),
		"MARKET_DATA_ENABLED":          "false",
		"MARKET_DATA_BHAVCOPY_ENABLED": "false",
		"GIN_MODE":                     "release",
	})

	fmt.Printf("starting API on %s...\n", baseURL)
	apiCmd := exec.Command(apiExe)
	apiCmd.Dir = repoRoot
	apiCmd.Env = apiEnv
	apiCmd.Stdout = apiOut
	apiCmd.Stderr = apiErr
	if err := apiCmd.Start(); err != nil {
		return fmt.Errorf("start API: %w", err)
	}

	apiDone := make(chan struct{})
	var apiWaitErr error
	go func() {
		apiWaitErr = apiCmd.Wait()
		close(apiDone)
	}()

	defer func() {
		if apiCmd.Process != nil {
			_ = apiCmd.Process.Kill()
		}
		select {
		case <-apiDone:
		case <-time.After(5 * time.Second):
		}

		// Closing the API's sockets does not always cancel a CPU-bound query;
		// PostgreSQL may keep scanning until the current statement finishes.
		// Terminate anything still running so a short benchmark does not leave
		// the machine saturated after the run ends.
		_ = runCommand(dbtoolExe, dbtoolArgs(opts, "stop-sessions"), repoRoot, baseEnv, io.Discard, io.Discard)
	}()

	if err := waitHealthy(baseURL+"/health", apiDone, &apiWaitErr, 120*time.Second); err != nil {
		return fmt.Errorf("%w (see %s and %s)", err, apiErrPath, apiOutPath)
	}

	// -----------------------------------------------------------------------
	// Seed
	// -----------------------------------------------------------------------

	if opts.skipSeed {
		fmt.Println("skipping seed (-skip-seed)")
	} else {
		fmt.Println("seeding benchmark data...")
		seedArgs := dbtoolArgs(opts, "seed", "-users", strconv.Itoa(opts.users))
		if err := runCommand(dbtoolExe, seedArgs, repoRoot, baseEnv, os.Stdout, os.Stderr); err != nil {
			return fmt.Errorf("seed: %w", err)
		}
	}

	fmt.Println("rebuilding read projections...")
	if err := runCommand(rebuildExe, []string{"--online"}, repoRoot, baseEnv, os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("rebuild projections: %w", err)
	}

	if err := runCommand(dbtoolExe, dbtoolArgs(opts, "counts", "-out", filepath.Join(resultDir, "row-counts.txt")), repoRoot, baseEnv, os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("row counts: %w", err)
	}

	// -----------------------------------------------------------------------
	// Query plans
	// -----------------------------------------------------------------------

	fmt.Println("capturing EXPLAIN plans...")
	plansPath := filepath.Join(resultDir, "plans.txt")
	if opts.skipPlans {
		fmt.Println("skipping EXPLAIN capture (-skip-plans)")
	} else if err := runCommand(dbtoolExe, dbtoolArgs(opts, "explain", "-out", plansPath, "-user-id", "1", "-explain-timeout", opts.explainTimeout.String()), repoRoot, baseEnv, os.Stdout, os.Stderr); err != nil {
		// A plan that exceeds its timeout is itself a baseline finding. Keep the
		// partial plans and continue to the load test instead of aborting.
		fmt.Printf("warning: EXPLAIN capture incomplete: %v\n", err)
	}

	// -----------------------------------------------------------------------
	// Load test
	// -----------------------------------------------------------------------

	if opts.skipLoad {
		fmt.Println("skipping load test (-skip-load)")
	} else {
		if err := runCommand(dbtoolExe, dbtoolArgs(opts, "reset-stats"), repoRoot, baseEnv, os.Stdout, io.Discard); err != nil {
			fmt.Printf("warning: could not reset pg_stat_statements: %v\n", err)
		}

		loadArgs := []string{
			"-base-url", baseURL,
			"-users", strconv.Itoa(opts.users),
			"-out", resultDir,
		}
		if opts.onlyPeak {
			loadArgs = append(loadArgs, "-warmup", "0s", "-steady", "0s", "-peak", "1m")
		} else if opts.quick {
			loadArgs = append(loadArgs, "-quick")
		}
		fmt.Println("running load test...")
		if err := runCommand(loadgenExe, loadArgs, repoRoot, baseEnv, os.Stdout, os.Stderr); err != nil {
			return fmt.Errorf("load test: %w", err)
		}
	}

	// -----------------------------------------------------------------------
	// Statistics
	// -----------------------------------------------------------------------

	fmt.Println("capturing database statistics...")
	if err := runCommand(dbtoolExe, dbtoolArgs(opts, "settings", "-out", filepath.Join(resultDir, "database-settings.txt")), repoRoot, baseEnv, os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("database settings: %w", err)
	}
	if err := runCommand(dbtoolExe, dbtoolArgs(opts, "activity", "-out", filepath.Join(resultDir, "db-activity.txt")), repoRoot, baseEnv, os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("database activity: %w", err)
	}
	if err := runCommand(dbtoolExe, dbtoolArgs(opts, "stats", "-out", filepath.Join(resultDir, "pg-stat-statements.txt")), repoRoot, baseEnv, os.Stdout, os.Stderr); err != nil {
		fmt.Printf("warning: pg_stat_statements unavailable: %v\n", err)
	}

	runInfo := fmt.Sprintf(
		"database=%s\nhost=%s\nport=%d\nusers=%d\nquick=%t\nonly_peak=%t\nload_skipped=%t\nstarted=%s\n",
		opts.database, baseURL, opts.port, opts.users, opts.quick, opts.onlyPeak, opts.skipLoad,
		time.Now().Format(time.RFC3339),
	)
	if err := os.WriteFile(filepath.Join(resultDir, "run-info.txt"), []byte(runInfo), 0o644); err != nil {
		return err
	}

	fmt.Printf("\nPhase 0 baseline complete: %s\n", resultDir)
	if summary, err := os.ReadFile(filepath.Join(resultDir, "summary.txt")); err == nil {
		fmt.Print(string(summary))
	}
	return nil
}

func build(repoRoot, output, pkg string) error {
	fmt.Printf("building %s...\n", pkg)
	cmd := exec.Command("go", "build", "-o", output, pkg)
	cmd.Dir = repoRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build %s: %w", pkg, err)
	}
	return nil
}

func runCommand(name string, args []string, dir string, env []string, stdout, stderr io.Writer) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func dbtoolArgs(opts options, command string, extra ...string) []string {
	args := []string{"-database", opts.database}
	if opts.dbHost != "" {
		args = append(args, "-host", opts.dbHost)
	}
	if opts.dbPort != "" {
		args = append(args, "-port", opts.dbPort)
	}
	if opts.dbUser != "" {
		args = append(args, "-user", opts.dbUser)
	}
	args = append(args, extra...)
	args = append(args, command)
	return args
}

func waitHealthy(url string, done <-chan struct{}, waitErr *error, timeout time.Duration) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return fmt.Errorf("API exited before becoming healthy: %v", *waitErr)
		default:
		}

		resp, err := client.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", url)
}

func withEnv(base []string, overrides map[string]string) []string {
	normalized := make(map[string]string, len(overrides))
	for key, value := range overrides {
		normalized[strings.ToUpper(key)] = value
	}

	result := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key := entry
		if index := strings.IndexByte(entry, '='); index >= 0 {
			key = entry[:index]
		}
		if _, replaced := normalized[strings.ToUpper(key)]; replaced {
			continue
		}
		result = append(result, entry)
	}
	for key, value := range normalized {
		result = append(result, key+"="+value)
	}
	return result
}

func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}
