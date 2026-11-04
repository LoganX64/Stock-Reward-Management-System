// Command phase0 runs the Stocky load-test harness without PowerShell.
//
// It builds the API, dbtool, and load generator; creates and seeds the
// benchmark database; starts one or more API processes; captures query plans;
// runs the configured load phases; captures database statistics; and stops the
// processes.
//
// The default profile is the original Phase 0 baseline. Phase 6 adds the
// steady-hour and peak-2x-hour profiles plus optional multi-instance routing
// through bench/lb.
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
	"sync"
	"time"
)

type options struct {
	database       string
	dbHost         string
	dbPort         string
	dbUser         string
	port           int
	users          int
	workers        int
	profile        string
	quick          bool
	skipLoad       bool
	skipPlans      bool
	skipSeed       bool
	skipRebuild    bool
	onlyPeak       bool
	explainTimeout time.Duration
	outputRoot     string

	// Load overrides. They are applied after the profile preset. A value is
	// only treated as an override when the flag was explicitly set, so a
	// profile can still disable a phase with an explicit zero duration.
	warmup     time.Duration
	steady     time.Duration
	peak       time.Duration
	warmupRate float64
	steadyRate float64
	peakRate   float64

	appRole           string
	apiInstances      int
	lbPort            int
	lbStrategy        string
	dbMaxOpen         int
	dbMaxIdle         int
	apiGOMAXPROCS     int
	loadgenGOMAXPROCS int
	lbGOMAXPROCS      int

	reconcile              bool
	reconcileSamplePercent float64
	reconcileMaxUsers      int
	reconcileMaxDetails    int
	reconcileTimeout       time.Duration
}

type loadProfile struct {
	name       string
	warmup     time.Duration
	steady     time.Duration
	peak       time.Duration
	warmupRate float64
	steadyRate float64
	peakRate   float64
}

type managedProcess struct {
	name   string
	port   int
	cmd    *exec.Cmd
	done   chan struct{}
	stdout *os.File
	stderr *os.File

	mu      sync.Mutex
	waitErr error

	closeOnce sync.Once
}

func main() {
	var opts options
	flag.StringVar(&opts.database, "database", "stocky_bench", "benchmark database")
	flag.StringVar(&opts.dbHost, "db-host", "", "override database host")
	flag.StringVar(&opts.dbPort, "db-port", "", "override database port")
	flag.StringVar(&opts.dbUser, "db-user", "", "override database user")
	flag.IntVar(&opts.port, "port", 18080, "API port (additional instances use consecutive ports)")
	flag.IntVar(&opts.users, "users", 20000, "number of seeded users")
	flag.IntVar(&opts.workers, "workers", 512, "number of concurrent load-generator workers")
	flag.StringVar(&opts.profile, "profile", "", "load profile: full, quick, steady-hour, peak-2x-hour")
	flag.BoolVar(&opts.quick, "quick", false, "short smoke run (alias for -profile quick)")
	flag.BoolVar(&opts.skipLoad, "skip-load", false, "seed and capture plans without running the load test")
	flag.BoolVar(&opts.skipPlans, "skip-plans", false, "skip EXPLAIN capture and go straight to the load test")
	flag.BoolVar(&opts.skipSeed, "skip-seed", false, "reuse the existing benchmark data instead of reseeding")
	flag.BoolVar(&opts.skipRebuild, "skip-rebuild", false, "reuse the current projections instead of running rebuild-projections --online")
	flag.BoolVar(&opts.onlyPeak, "only-peak", false, "run only the 1,000 req/s peak phase for 1 minute")
	flag.DurationVar(&opts.warmup, "warmup", 0, "override warm-up duration (0 disables the phase)")
	flag.DurationVar(&opts.steady, "steady", 0, "override steady duration (0 disables the phase)")
	flag.DurationVar(&opts.peak, "peak", 0, "override peak duration (0 disables the phase)")
	flag.Float64Var(&opts.warmupRate, "warmup-rate", 0, "override warm-up requests per second")
	flag.Float64Var(&opts.steadyRate, "steady-rate", 0, "override steady requests per second")
	flag.Float64Var(&opts.peakRate, "peak-rate", 0, "override peak requests per second")
	flag.StringVar(&opts.appRole, "app-role", "all", "API role during the load: all or api (multi-instance forces api)")
	flag.IntVar(&opts.apiInstances, "api-instances", 1, "number of API processes; values above 1 start bench/lb")
	flag.IntVar(&opts.lbPort, "lb-port", 0, "load-balancer port (default: -port + -api-instances)")
	flag.StringVar(&opts.lbStrategy, "lb-strategy", "round-robin", "bench/lb strategy: round-robin or user")
	flag.IntVar(&opts.dbMaxOpen, "db-max-open-conns", 0, "override DB_MAX_OPEN_CONNS for API processes (0 keeps the app default)")
	flag.IntVar(&opts.dbMaxIdle, "db-max-idle-conns", 0, "override DB_MAX_IDLE_CONNS for API processes (0 keeps the app default)")
	flag.IntVar(&opts.apiGOMAXPROCS, "api-gomaxprocs", 0, "GOMAXPROCS for each API process (0 keeps the Go default)")
	flag.IntVar(&opts.loadgenGOMAXPROCS, "loadgen-gomaxprocs", 0, "GOMAXPROCS for the load generator (0 keeps the Go default)")
	flag.IntVar(&opts.lbGOMAXPROCS, "lb-gomaxprocs", 0, "GOMAXPROCS for bench/lb (0 keeps the Go default)")
	flag.BoolVar(&opts.reconcile, "reconcile", false, "run the projection reconciliation after the load test")
	flag.Float64Var(&opts.reconcileSamplePercent, "reconcile-sample-percent", 10, "reconciliation sample percent")
	flag.IntVar(&opts.reconcileMaxUsers, "reconcile-max-users", 2000, "maximum users to reconcile")
	flag.IntVar(&opts.reconcileMaxDetails, "reconcile-max-details", 1000, "maximum mismatch details to retain")
	flag.DurationVar(&opts.reconcileTimeout, "reconcile-timeout", 30*time.Minute, "reconciliation timeout")
	flag.DurationVar(&opts.explainTimeout, "explain-timeout", 10*time.Minute, "timeout for each EXPLAIN statement")
	flag.StringVar(&opts.outputRoot, "output-root", "bench/results", "results directory")
	flag.Parse()

	setFlags := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) {
		setFlags[f.Name] = true
	})

	if err := run(opts, setFlags); err != nil {
		fmt.Fprintf(os.Stderr, "phase0: %v\n", err)
		os.Exit(1)
	}
}

func run(opts options, setFlags map[string]bool) error {
	repoRoot, err := os.Getwd()
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err != nil {
		return fmt.Errorf("run from the repository root: %w", err)
	}

	profile, err := resolveLoadProfile(opts, setFlags)
	if err != nil {
		return err
	}
	if err := validateOptions(&opts); err != nil {
		return err
	}

	binDir := filepath.Join(repoRoot, "bench", "bin")
	resultDir := filepath.Join(repoRoot, opts.outputRoot, fmt.Sprintf("phase0-%s-%s", profile.name, time.Now().Format("20060102-150405")))
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
	lbExe := filepath.Join(binDir, exeName("lb"))

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
	if opts.apiInstances > 1 {
		if err := build(repoRoot, lbExe, "./bench/lb"); err != nil {
			return err
		}
	}

	baseEnv := withEnv(os.Environ(), map[string]string{
		"POSTGRES_DB": opts.database,
	})

	var processes []*managedProcess
	defer func() {
		for i := len(processes) - 1; i >= 0; i-- {
			processes[i].stop(5 * time.Second)
		}

		// Closing the API's sockets does not always cancel a CPU-bound query;
		// PostgreSQL may keep scanning until the current statement finishes.
		// Terminate anything still running so a short benchmark does not leave
		// the machine saturated after the run ends.
		_ = runCommand(dbtoolExe, dbtoolArgs(opts, "stop-sessions"), repoRoot, baseEnv, io.Discard, io.Discard)
	}()

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
	// API processes
	// -----------------------------------------------------------------------

	apiEnv := withEnv(os.Environ(), map[string]string{
		"POSTGRES_DB":                  opts.database,
		"HTTP_PORT":                    strconv.Itoa(opts.port),
		"MARKET_DATA_ENABLED":          "false",
		"MARKET_DATA_BHAVCOPY_ENABLED": "false",
		"GIN_MODE":                     "release",
		"APP_ROLE":                     opts.appRole,
		"RUN_MIGRATIONS_ON_START":      strconv.FormatBool(opts.appRole == "all"),
	})
	if opts.dbMaxOpen > 0 {
		apiEnv = withEnv(apiEnv, map[string]string{"DB_MAX_OPEN_CONNS": strconv.Itoa(opts.dbMaxOpen)})
	}
	if opts.dbMaxIdle > 0 {
		apiEnv = withEnv(apiEnv, map[string]string{"DB_MAX_IDLE_CONNS": strconv.Itoa(opts.dbMaxIdle)})
	}
	if opts.apiGOMAXPROCS > 0 {
		apiEnv = withEnv(apiEnv, map[string]string{"GOMAXPROCS": strconv.Itoa(opts.apiGOMAXPROCS)})
	}

	if opts.appRole == "api" {
		fmt.Println("applying migrations with APP_ROLE=migrate...")
		migrateEnv := withEnv(apiEnv, map[string]string{
			"APP_ROLE":                "migrate",
			"RUN_MIGRATIONS_ON_START": "true",
		})
		migrateProc, err := startProcess(
			"migrate",
			0,
			apiExe,
			nil,
			repoRoot,
			migrateEnv,
			filepath.Join(resultDir, "migrate.out.log"),
			filepath.Join(resultDir, "migrate.err.log"),
		)
		if err != nil {
			return fmt.Errorf("start migration process: %w", err)
		}
		if err := waitProcess(migrateProc, 5*time.Minute); err != nil {
			return fmt.Errorf("migrations: %w", err)
		}
		migrateProc.stop(0)
	}

	fmt.Printf("starting %d API process(es)...\n", opts.apiInstances)
	apiProcs := make([]*managedProcess, 0, opts.apiInstances)
	for i := 0; i < opts.apiInstances; i++ {
		apiPort := opts.port + i
		env := withEnv(apiEnv, map[string]string{
			"HTTP_PORT": strconv.Itoa(apiPort),
		})
		proc, err := startProcess(
			fmt.Sprintf("api-%d", apiPort),
			apiPort,
			apiExe,
			nil,
			repoRoot,
			env,
			filepath.Join(resultDir, fmt.Sprintf("api-%d.out.log", apiPort)),
			filepath.Join(resultDir, fmt.Sprintf("api-%d.err.log", apiPort)),
		)
		if err != nil {
			return fmt.Errorf("start API on port %d: %w", apiPort, err)
		}
		processes = append(processes, proc)
		apiProcs = append(apiProcs, proc)
	}

	for _, proc := range apiProcs {
		url := fmt.Sprintf("http://127.0.0.1:%d/health", proc.port)
		if err := waitHealthy(url, proc, 120*time.Second); err != nil {
			return fmt.Errorf("%w (see %s and %s)", err, proc.stdout.Name(), proc.stderr.Name())
		}
	}

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", opts.port)
	if opts.apiInstances > 1 {
		backends := make([]string, 0, len(apiProcs))
		for _, proc := range apiProcs {
			backends = append(backends, fmt.Sprintf("http://127.0.0.1:%d", proc.port))
		}
		lbArgs := []string{
			"-listen", fmt.Sprintf("127.0.0.1:%d", opts.lbPort),
			"-backends", strings.Join(backends, ","),
			"-strategy", opts.lbStrategy,
			"-health-interval", "5s",
			"-health-timeout", "5s",
		}
		lbProc, err := startProcess(
			"lb",
			opts.lbPort,
			lbExe,
			lbArgs,
			repoRoot,
			withGOMAXPROCS(os.Environ(), opts.lbGOMAXPROCS),
			filepath.Join(resultDir, "lb.out.log"),
			filepath.Join(resultDir, "lb.err.log"),
		)
		if err != nil {
			return fmt.Errorf("start load balancer: %w", err)
		}
		processes = append(processes, lbProc)
		baseURL = fmt.Sprintf("http://127.0.0.1:%d", opts.lbPort)
		if err := waitHealthy(baseURL+"/health", lbProc, 30*time.Second); err != nil {
			return fmt.Errorf("load balancer: %w (see %s and %s)", err, lbProc.stdout.Name(), lbProc.stderr.Name())
		}
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

	if opts.skipRebuild {
		fmt.Println("skipping projection rebuild (-skip-rebuild)")
	} else {
		fmt.Println("rebuilding read projections...")
		if err := runCommand(rebuildExe, []string{"--online"}, repoRoot, baseEnv, os.Stdout, os.Stderr); err != nil {
			return fmt.Errorf("rebuild projections: %w", err)
		}
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
			"-workers", strconv.Itoa(opts.workers),
			"-warmup", profile.warmup.String(),
			"-steady", profile.steady.String(),
			"-peak", profile.peak.String(),
			"-warmup-rate", strconv.FormatFloat(profile.warmupRate, 'f', -1, 64),
			"-steady-rate", strconv.FormatFloat(profile.steadyRate, 'f', -1, 64),
			"-peak-rate", strconv.FormatFloat(profile.peakRate, 'f', -1, 64),
			"-out", resultDir,
		}
		fmt.Println("running load test...")
		if err := runCommand(loadgenExe, loadArgs, repoRoot, withGOMAXPROCS(baseEnv, opts.loadgenGOMAXPROCS), os.Stdout, os.Stderr); err != nil {
			return fmt.Errorf("load test: %w", err)
		}
	}

	// -----------------------------------------------------------------------
	// Reconciliation
	// -----------------------------------------------------------------------

	if opts.reconcile {
		fmt.Println("running projection reconciliation...")
		reconcileArgs := dbtoolArgs(
			opts,
			"reconcile",
			"-sample-percent", strconv.FormatFloat(opts.reconcileSamplePercent, 'f', -1, 64),
			"-max-users", strconv.Itoa(opts.reconcileMaxUsers),
			"-max-details", strconv.Itoa(opts.reconcileMaxDetails),
			"-reconcile-timeout", opts.reconcileTimeout.String(),
			"-out", filepath.Join(resultDir, "reconciliation.txt"),
		)
		if err := runCommand(dbtoolExe, reconcileArgs, repoRoot, baseEnv, os.Stdout, os.Stderr); err != nil {
			return fmt.Errorf("reconciliation: %w", err)
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
	if err := runCommand(dbtoolExe, dbtoolArgs(opts, "stats", "-out", filepath.Join(resultDir, "pg-stat-statements.txt")), repoRoot, baseEnv, os.Stdout, io.Discard); err != nil {
		fmt.Printf("warning: pg_stat_statements unavailable: %v\n", err)
	}

	fmt.Println("capturing API metrics...")
	for _, proc := range apiProcs {
		name := "api-metrics.txt"
		if len(apiProcs) > 1 {
			name = fmt.Sprintf("api-metrics-%d.txt", proc.port)
		}
		url := fmt.Sprintf("http://127.0.0.1:%d", proc.port)
		if err := captureMetrics(url, filepath.Join(resultDir, name)); err != nil {
			fmt.Printf("warning: could not capture %s/metrics: %v\n", url, err)
		}
	}
	if opts.apiInstances > 1 {
		if err := captureURL(baseURL+"/lb/backends", filepath.Join(resultDir, "lb-backends.json")); err != nil {
			fmt.Printf("warning: could not capture load-balancer backends: %v\n", err)
		}
	}

	runInfo := fmt.Sprintf(
		"database=%s\nhost=%s\nport=%d\nusers=%d\nworkers=%d\nprofile=%s\nwarmup=%s\nsteady=%s\npeak=%s\nwarmup_rate=%.0f\nsteady_rate=%.0f\npeak_rate=%.0f\napp_role=%s\napi_instances=%d\nlb_port=%d\nlb_strategy=%s\ndb_max_open=%d\ndb_max_idle=%d\napi_gomaxprocs=%d\nloadgen_gomaxprocs=%d\nlb_gomaxprocs=%d\nskip_seed=%t\nskip_rebuild=%t\nskip_plans=%t\nreconcile=%t\nreconcile_sample_percent=%.2f\nreconcile_max_users=%d\nload_skipped=%t\nstarted=%s\n",
		opts.database, baseURL, opts.port, opts.users, opts.workers, profile.name,
		profile.warmup, profile.steady, profile.peak,
		profile.warmupRate, profile.steadyRate, profile.peakRate,
		opts.appRole, opts.apiInstances, opts.lbPort, opts.lbStrategy,
		opts.dbMaxOpen, opts.dbMaxIdle,
		opts.apiGOMAXPROCS, opts.loadgenGOMAXPROCS, opts.lbGOMAXPROCS,
		opts.skipSeed, opts.skipRebuild, opts.skipPlans, opts.reconcile,
		opts.reconcileSamplePercent, opts.reconcileMaxUsers, opts.skipLoad,
		time.Now().Format(time.RFC3339),
	)
	if err := os.WriteFile(filepath.Join(resultDir, "run-info.txt"), []byte(runInfo), 0o644); err != nil {
		return err
	}

	fmt.Printf("\nLoad run complete: %s\n", resultDir)
	if summary, err := os.ReadFile(filepath.Join(resultDir, "summary.txt")); err == nil {
		fmt.Print(string(summary))
	}
	return nil
}

func resolveLoadProfile(opts options, setFlags map[string]bool) (loadProfile, error) {
	profile := loadProfile{
		name:       "full",
		warmup:     60 * time.Second,
		steady:     10 * time.Minute,
		peak:       5 * time.Minute,
		warmupRate: 50,
		steadyRate: 278,
		peakRate:   1000,
	}

	switch opts.profile {
	case "", "full":
	case "quick":
		profile = quickProfile()
	case "steady-hour":
		profile = loadProfile{
			name:       "steady-hour",
			warmup:     60 * time.Second,
			steady:     60 * time.Minute,
			warmupRate: 50,
			steadyRate: 278,
		}
	case "peak-2x-hour":
		profile = loadProfile{
			name:       "peak-2x-hour",
			warmup:     60 * time.Second,
			peak:       60 * time.Minute,
			warmupRate: 50,
			peakRate:   2000,
		}
	default:
		return profile, fmt.Errorf("unknown -profile %q (want full, quick, steady-hour, or peak-2x-hour)", opts.profile)
	}

	if opts.quick {
		if opts.profile != "" && opts.profile != "quick" {
			return profile, fmt.Errorf("-quick conflicts with -profile %q", opts.profile)
		}
		profile = quickProfile()
	}
	if opts.onlyPeak {
		if opts.quick {
			return profile, fmt.Errorf("-only-peak conflicts with -quick")
		}
		if opts.profile == "steady-hour" || opts.profile == "peak-2x-hour" {
			return profile, fmt.Errorf("-only-peak conflicts with -profile %q", opts.profile)
		}
		profile = loadProfile{
			name:       "peak-only",
			peak:       time.Minute,
			warmupRate: 50,
			steadyRate: 278,
			peakRate:   1000,
		}
	}

	if setFlags["warmup"] {
		profile.warmup = opts.warmup
	}
	if setFlags["steady"] {
		profile.steady = opts.steady
	}
	if setFlags["peak"] {
		profile.peak = opts.peak
	}
	if setFlags["warmup-rate"] {
		profile.warmupRate = opts.warmupRate
	}
	if setFlags["steady-rate"] {
		profile.steadyRate = opts.steadyRate
	}
	if setFlags["peak-rate"] {
		profile.peakRate = opts.peakRate
	}

	if profile.warmup < 0 || profile.steady < 0 || profile.peak < 0 {
		return profile, fmt.Errorf("phase durations must not be negative")
	}
	if profile.warmupRate < 0 || profile.steadyRate < 0 || profile.peakRate < 0 {
		return profile, fmt.Errorf("phase rates must not be negative")
	}
	if !opts.skipLoad {
		active := 0
		if profile.warmup > 0 && profile.warmupRate > 0 {
			active++
		}
		if profile.steady > 0 && profile.steadyRate > 0 {
			active++
		}
		if profile.peak > 0 && profile.peakRate > 0 {
			active++
		}
		if active == 0 {
			return profile, fmt.Errorf("no active load phase: set a positive duration and rate")
		}
	}

	return profile, nil
}

func quickProfile() loadProfile {
	return loadProfile{
		name:       "quick",
		warmup:     30 * time.Second,
		steady:     2 * time.Minute,
		peak:       time.Minute,
		warmupRate: 50,
		steadyRate: 278,
		peakRate:   1000,
	}
}

func validateOptions(opts *options) error {
	if opts.users < 1 {
		return fmt.Errorf("-users must be at least 1")
	}
	if opts.workers < 1 {
		return fmt.Errorf("-workers must be at least 1")
	}
	if opts.apiInstances < 1 {
		return fmt.Errorf("-api-instances must be at least 1")
	}
	if opts.port < 1 || opts.port > 65535 {
		return fmt.Errorf("-port must be between 1 and 65535")
	}
	if opts.port+opts.apiInstances-1 > 65535 {
		return fmt.Errorf("-port + -api-instances exceeds port 65535")
	}
	if opts.appRole != "all" && opts.appRole != "api" {
		return fmt.Errorf("-app-role must be all or api")
	}
	if opts.apiInstances > 1 && opts.appRole == "all" {
		// Multiple "all" processes would duplicate background jobs. Route the
		// load through API-only processes after a single migration pass.
		opts.appRole = "api"
	}
	if opts.apiInstances > 1 {
		if opts.lbPort == 0 {
			opts.lbPort = opts.port + opts.apiInstances
		}
		if opts.lbPort < 1 || opts.lbPort > 65535 {
			return fmt.Errorf("-lb-port must be between 1 and 65535")
		}
		if opts.lbPort >= opts.port && opts.lbPort < opts.port+opts.apiInstances {
			return fmt.Errorf("-lb-port %d overlaps an API port (%d..%d)", opts.lbPort, opts.port, opts.port+opts.apiInstances-1)
		}
		if opts.lbPort == opts.port {
			return fmt.Errorf("-lb-port must differ from -port")
		}
		if opts.lbStrategy != "round-robin" && opts.lbStrategy != "user" {
			return fmt.Errorf("-lb-strategy must be round-robin or user")
		}
	}
	if opts.reconcile {
		if opts.reconcileSamplePercent <= 0 || opts.reconcileSamplePercent > 100 {
			return fmt.Errorf("-reconcile-sample-percent must be greater than 0 and at most 100")
		}
		if opts.reconcileMaxUsers < 1 {
			return fmt.Errorf("-reconcile-max-users must be at least 1")
		}
		if opts.reconcileMaxDetails < 0 {
			return fmt.Errorf("-reconcile-max-details must not be negative")
		}
		if opts.reconcileTimeout <= 0 {
			return fmt.Errorf("-reconcile-timeout must be greater than zero")
		}
	}
	if opts.dbMaxOpen < 0 {
		return fmt.Errorf("-db-max-open-conns must not be negative")
	}
	if opts.dbMaxIdle < 0 {
		return fmt.Errorf("-db-max-idle-conns must not be negative")
	}
	if opts.dbMaxOpen > 0 && opts.dbMaxIdle > opts.dbMaxOpen {
		return fmt.Errorf("-db-max-idle-conns must not exceed -db-max-open-conns")
	}
	if opts.apiGOMAXPROCS < 0 || opts.loadgenGOMAXPROCS < 0 || opts.lbGOMAXPROCS < 0 {
		return fmt.Errorf("GOMAXPROCS overrides must not be negative")
	}
	return nil
}

func startProcess(name string, port int, exe string, args []string, dir string, env []string, stdoutPath, stderrPath string) (*managedProcess, error) {
	stdout, err := os.Create(stdoutPath)
	if err != nil {
		return nil, err
	}
	stderr, err := os.Create(stderrPath)
	if err != nil {
		_ = stdout.Close()
		return nil, err
	}

	cmd := exec.Command(exe, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, err
	}

	proc := &managedProcess{
		name:   name,
		port:   port,
		cmd:    cmd,
		done:   make(chan struct{}),
		stdout: stdout,
		stderr: stderr,
	}
	go func() {
		err := cmd.Wait()
		proc.mu.Lock()
		proc.waitErr = err
		proc.mu.Unlock()
		close(proc.done)
	}()
	return proc, nil
}

func (p *managedProcess) stop(timeout time.Duration) {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	select {
	case <-p.done:
	case <-time.After(timeout):
	}
	p.closeOnce.Do(func() {
		_ = p.stdout.Close()
		_ = p.stderr.Close()
	})
}

func (p *managedProcess) waitErrValue() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitErr
}

func waitProcess(p *managedProcess, timeout time.Duration) error {
	select {
	case <-p.done:
		return p.waitErrValue()
	case <-time.After(timeout):
		p.stop(0)
		return fmt.Errorf("%s timed out after %s", p.name, timeout)
	}
}

func waitHealthy(url string, proc *managedProcess, timeout time.Duration) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-proc.done:
			return fmt.Errorf("%s exited before becoming healthy: %v", proc.name, proc.waitErrValue())
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

func captureMetrics(baseURL, path string) error {
	return captureURL(strings.TrimRight(baseURL, "/")+"/metrics", path)
}

func captureURL(url, path string) error {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
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

func withGOMAXPROCS(base []string, value int) []string {
	if value <= 0 {
		return base
	}
	return withEnv(base, map[string]string{"GOMAXPROCS": strconv.Itoa(value)})
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
