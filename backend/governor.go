package backend

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	onepassword "github.com/1password/onepassword-sdk-go"
)

// govState is the governor's engine-wide (not per-vault) state
// machine, spec §4 Failure behaviour.
type govState int

const (
	govStateNormal govState = iota
	govStateRateLimited
	govStateAuthFailed
)

func (s govState) String() string {
	switch s {
	case govStateRateLimited:
		return "rate_limited"
	case govStateAuthFailed:
		return "auth_failed"
	default:
		return "normal"
	}
}

// burstBrakeFraction is the spec §4 burst brake threshold: 80% of
// hourly_read_limit defers deferrable (workClassPeriodic) work.
const burstBrakeFraction = 0.8

// probeTimeout bounds how long a ratelimit_probe_cmd exec is allowed
// to run before it's treated as a probe failure (fail-safe to local
// counters, spec D12).
const probeTimeout = 10 * time.Second

// vaultBackoff tracks per-vault exponential backoff after an
// unclassified (non-429/non-auth) cycle error (spec §4: "exponential
// backoff, capped at refresh_interval, for other errors").
type vaultBackoff struct {
	consecutiveFailures int
	nextAllowedAt       time.Time
}

// clientInitState tracks retry state for 1P SDK client construction
// (spec §4 Restart): the SDK client factory performs a live auth
// handshake, so it can fail independently of config load — e.g. a
// plugin restart while 1Password is unreachable. Engine-wide (there is
// only ever one client) rather than per-vault, but paced with the same
// exponential backoff a vault's cycle failures use (backoffDelay), via
// clientInitAllowed/recordClientInitResult below.
type clientInitState struct {
	consecutiveFailures int
	nextAllowedAt       time.Time
	lastErr             string
}

// governorSnapshot is the read-only view of governor state exposed to
// op/status (Task 6's paths_status.go) and to read-path failure
// fallback decisions.
type governorSnapshot struct {
	State          string
	ResumeAt       time.Time
	Throttled      bool // burst brake currently deferring periodic work
	HourlyUsagePct int
	DailyUsagePct  int
	UsagePct       int // spec §4/D12 ceiling input: max(hourly,daily,probed)

	ProbeConfigured  bool
	ProbeHealthy     bool
	ProbeErr         string
	ProbeWarning     string
	ProbedAccountPct int
	LastProbeAt      time.Time

	// Backoff is a snapshot of per-vault backoff state, vaultID ->
	// consecutive failure count (0 = no active backoff).
	Backoff map[string]int

	// ClientInitFailures/ClientInitLastErr surface clientInitState
	// (spec §4 Restart) on op/status — visible proof that a client-
	// construction failure (e.g. cold start during a 1P outage) is
	// being retried, not silently stuck. Zero/empty means the client
	// is constructed or hasn't been attempted yet.
	ClientInitFailures int
	ClientInitLastErr  string
}

// governor is the spec §4/§3/D12 rate governor: rolling local usage
// counters, the passthrough ceiling function, the hourly burst brake,
// 429/auth_failed state handling, per-vault backoff, and the optional
// account-wide usage probe. It implements requestGate (cycle.go) and
// is the concrete type of Backend.gate.
type governor struct {
	mu  sync.Mutex
	now func() time.Time

	hourlyLimit     int
	dailyLimit      int
	refreshInterval time.Duration

	// events holds one timestamp per counted request-budget unit
	// (spec: "every 1P call counts"), pruned to the last 24h on every
	// record/read so hourly and daily rolling counts can both be
	// derived from a single slice.
	events []time.Time

	state    govState
	resumeAt time.Time

	backoff map[string]*vaultBackoff

	// clientInit tracks 1P SDK client-construction retry state
	// (clientInitAllowed/recordClientInitResult below) — engine-wide,
	// unlike backoff's per-vault map, since there is only one client.
	clientInit clientInitState

	probeCmd string
	token    string

	lastProbeAt      time.Time
	probeHealthy     bool
	probedAccountPct float64
	probeErr         string
	probeWarning     string
}

var _ requestGate = (*governor)(nil)

func newGovernor(now func() time.Time) *governor {
	return &governor{
		now:     now,
		backoff: map[string]*vaultBackoff{},
	}
}

// reconfigure applies the limits, probe command, and token from a
// successful config write. Spec §4 Failure behaviour: "auth_failed
// ... clear[s] only on a successful config rewrite" — this is that
// hook, wired in from config.go's applyConfig. rate_limited is
// time-based (a 429's advertised horizon) and is deliberately left
// alone by a config rewrite; only auth_failed is config-cleared.
// clientInit is also cleared: applyConfig constructs a fresh client
// synchronously and sets it directly, so any prior client-construction
// retry state is stale as of this call.
func (g *governor) reconfigure(cfg *Config) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.hourlyLimit = cfg.HourlyReadLimit
	g.dailyLimit = cfg.DailyRequestLimit
	g.refreshInterval = cfg.RefreshInterval
	g.probeCmd = cfg.RatelimitProbeCmd
	g.token = cfg.ServiceAccountToken
	if g.state == govStateAuthFailed {
		g.state = govStateNormal
	}
	g.clientInit = clientInitState{}
}

// clientInitAllowed reports whether a 1P SDK client-construction
// attempt may run right now: blocked while auth_failed (a config
// rewrite is required to clear that, same as any other engine-wide
// auth failure) or rate_limited (until resumeAt, cleared here exactly
// like allow() clears it for cycles), and otherwise paced by
// clientInitState's own backoff window.
func (g *governor) clientInitAllowed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()

	switch g.state {
	case govStateRateLimited:
		if !now.Before(g.resumeAt) {
			g.state = govStateNormal
			g.resumeAt = time.Time{}
		} else {
			return false
		}
	case govStateAuthFailed:
		return false
	}

	return !now.Before(g.clientInit.nextAllowedAt)
}

// recordClientInitResult classifies a client-construction outcome the
// same way recordRequest classifies a cycle failure (spec §4 Failure
// behaviour): a rate-limited or auth-failed handshake sets the same
// engine-wide state a cycle failure would (auth_failed then requires a
// config rewrite to clear — see reconfigure). Anything else — e.g. the
// bench 7b scenario, a plain connectivity failure while 1Password is
// unreachable — paces retries via clientInitState's own exponential
// backoff (backoffDelay), mirroring vaultBackoff but engine-wide since
// there is only one client. A nil err (successful construction) clears
// all retry state.
func (g *governor) recordClientInitResult(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err == nil {
		g.clientInit = clientInitState{}
		return
	}

	switch classifyError(err) {
	case errClassRateLimited:
		g.state = govStateRateLimited
		g.resumeAt = rateLimitResumeAt(err, g.now())
	case errClassAuthFailed:
		g.state = govStateAuthFailed
	default:
		g.clientInit.consecutiveFailures++
		g.clientInit.lastErr = err.Error()
		g.clientInit.nextAllowedAt = g.now().Add(backoffDelay(g.clientInit.consecutiveFailures, g.refreshInterval))
	}
}

// allow implements requestGate: rate_limited/auth_failed halt every
// class; a vault in backoff halts every class for that vault; the
// burst brake defers only workClassPeriodic once hourly usage crosses
// burstBrakeFraction, and denies every class once it reaches the
// hourly limit outright (the "hard cap" miss-triggered and manual
// work still respects, spec §4).
func (g *governor) allow(vaultID string, class workClass) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()

	switch g.state {
	case govStateRateLimited:
		if !now.Before(g.resumeAt) {
			g.state = govStateNormal
			g.resumeAt = time.Time{}
		} else {
			return false
		}
	case govStateAuthFailed:
		return false
	}

	if b, ok := g.backoff[vaultID]; ok && now.Before(b.nextAllowedAt) {
		return false
	}

	if g.hourlyLimit > 0 {
		g.pruneLocked(now)
		hourly := g.countSinceLocked(now, time.Hour)
		switch {
		case hourly >= g.hourlyLimit:
			return false
		case class == workClassPeriodic && float64(hourly) >= burstBrakeFraction*float64(g.hourlyLimit):
			return false
		}
	}

	return true
}

// recordRequest implements requestGate: counts units toward the
// rolling local counters and, on error, classifies it into a state
// transition (spec §4 Failure behaviour). A failed call still counts
// as a request (matches the FakeOPClient convention from Task 2/4).
func (g *governor) recordRequest(vaultID string, units int, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	for i := 0; i < units; i++ {
		g.events = append(g.events, now)
	}
	g.pruneLocked(now)

	if err == nil {
		return
	}

	switch classifyError(err) {
	case errClassRateLimited:
		g.state = govStateRateLimited
		g.resumeAt = rateLimitResumeAt(err, now)
	case errClassAuthFailed:
		g.state = govStateAuthFailed
	default:
		b, ok := g.backoff[vaultID]
		if !ok {
			b = &vaultBackoff{}
			g.backoff[vaultID] = b
		}
		b.consecutiveFailures++
		b.nextAllowedAt = now.Add(backoffDelay(b.consecutiveFailures, g.refreshInterval))
	}
}

// recordSuccess implements requestGate: clears vaultID's backoff
// state after a fully successful cycle.
func (g *governor) recordSuccess(vaultID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.backoff, vaultID)
}

// pruneLocked drops events older than 24h — the longest rolling
// window the governor tracks. Caller must hold g.mu.
func (g *governor) pruneLocked(now time.Time) {
	cutoff := now.Add(-24 * time.Hour)
	i := 0
	for ; i < len(g.events); i++ {
		if g.events[i].After(cutoff) {
			break
		}
	}
	if i > 0 {
		g.events = g.events[i:]
	}
}

// countSinceLocked counts events within window of now. Caller must
// hold g.mu and have already pruned.
func (g *governor) countSinceLocked(now time.Time, window time.Duration) int {
	cutoff := now.Add(-window)
	n := 0
	for _, t := range g.events {
		if t.After(cutoff) {
			n++
		}
	}
	return n
}

// usagePct is the spec §4/D12 ceiling input:
// usage_pct = max(local_counter_pct, probed_account_pct), where
// local_counter_pct itself is max(hourly_pct, daily_pct) — "both
// [hourly and daily] below passthrough_ceiling_pct" is exactly "max
// of the two is below the ceiling". The probe can only ever raise
// this number, never lower it (spec: "the probe can only tighten the
// gate, never loosen it below what local counters show").
func (g *governor) usagePct() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.pruneLocked(now)

	hourlyPct := pctOf(g.countSinceLocked(now, time.Hour), g.hourlyLimit)
	dailyPct := pctOf(g.countSinceLocked(now, 24*time.Hour), g.dailyLimit)
	pct := hourlyPct
	if dailyPct > pct {
		pct = dailyPct
	}
	if g.probeHealthy {
		probedPct := int(g.probedAccountPct)
		if probedPct > pct {
			pct = probedPct
		}
	}
	return pct
}

// snapshot returns the read-only view for op/status and read-path
// fallback decisions.
func (g *governor) snapshot() governorSnapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.pruneLocked(now)

	hourlyPct := pctOf(g.countSinceLocked(now, time.Hour), g.hourlyLimit)
	dailyPct := pctOf(g.countSinceLocked(now, 24*time.Hour), g.dailyLimit)
	pct := hourlyPct
	if dailyPct > pct {
		pct = dailyPct
	}
	probedPct := int(g.probedAccountPct)
	if g.probeHealthy && probedPct > pct {
		pct = probedPct
	}

	throttled := g.hourlyLimit > 0 && float64(g.countSinceLocked(now, time.Hour)) >= burstBrakeFraction*float64(g.hourlyLimit)

	backoff := make(map[string]int, len(g.backoff))
	for id, b := range g.backoff {
		if now.Before(b.nextAllowedAt) {
			backoff[id] = b.consecutiveFailures
		}
	}

	return governorSnapshot{
		State:            g.state.String(),
		ResumeAt:         g.resumeAt,
		Throttled:        throttled,
		HourlyUsagePct:   hourlyPct,
		DailyUsagePct:    dailyPct,
		UsagePct:         pct,
		ProbeConfigured:  g.probeCmd != "",
		ProbeHealthy:     g.probeHealthy,
		ProbeErr:         g.probeErr,
		ProbeWarning:     g.probeWarning,
		ProbedAccountPct: probedPct,
		LastProbeAt:      g.lastProbeAt,
		Backoff:          backoff,

		ClientInitFailures: g.clientInit.consecutiveFailures,
		ClientInitLastErr:  g.clientInit.lastErr,
	}
}

// isVaultFailing reports whether vaultID is currently unable to
// refresh: engine-wide rate_limited/auth_failed, or this vault's own
// backoff window. Used by the read path (Task 6) to decide whether a
// serve is "stale" in the D6 outage sense.
func (g *governor) isVaultFailing(vaultID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if g.state != govStateNormal {
		if g.state == govStateRateLimited && !now.Before(g.resumeAt) {
			// Expired; allow() will clear it on the next attempt.
			return false
		}
		return true
	}
	if b, ok := g.backoff[vaultID]; ok && now.Before(b.nextAllowedAt) {
		return true
	}
	return false
}

// pctOf returns count/limit as a percentage; an unconfigured (<=0)
// limit contributes nothing (0%) rather than dividing by zero.
func pctOf(count, limit int) int {
	if limit <= 0 {
		return 0
	}
	return count * 100 / limit
}

// errClass classifies a 1Password SDK error for governor state
// transitions.
type errClass int

const (
	errClassOther errClass = iota
	errClassRateLimited
	errClassAuthFailed
)

// authFailureNeedles are substrings of the 1Password SDK's actual
// error messages for 401/403-shaped failures. The SDK (v0.4.1,
// verified 2026-08-05 via `strings` on the embedded WASM core) has no
// typed error for these — only RateLimitExceededError and
// DesktopSessionExpiredError are typed (errors.go); everything else,
// including auth failures, comes back as a plain error whose message
// is one of the WASM core's fixed strings ("you are not
// authenticated", "you don't have the right permissions to access
// this resource", "bad service account token, please rotate it").
var authFailureNeedles = []string{
	"not authenticated",
	"right permissions",
	"bad service account token",
	"forbidden",
	"unauthorized",
}

func classifyError(err error) errClass {
	var rle *onepassword.RateLimitExceededError
	if errors.As(err, &rle) {
		return errClassRateLimited
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range authFailureNeedles {
		if strings.Contains(msg, needle) {
			return errClassAuthFailed
		}
	}
	return errClassOther
}

// retryHorizonRe defensively parses a "retry in N <unit>" horizon out
// of a rate-limit error message, in case a future SDK version embeds
// one. The v0.4.1 RateLimitExceededError message is the fixed string
// "rate limit exceeded" with no horizon (verified 2026-08-05), so this
// never matches today — rateLimitResumeAt falls back to top-of-hour,
// per spec §4's documented fallback.
var retryHorizonRe = regexp.MustCompile(`(?i)retry in (\d+)\s*(second|minute|hour)s?`)

func rateLimitResumeAt(err error, now time.Time) time.Time {
	if m := retryHorizonRe.FindStringSubmatch(err.Error()); m != nil {
		if n, e := strconv.Atoi(m[1]); e == nil {
			var unit time.Duration
			switch strings.ToLower(m[2]) {
			case "second":
				unit = time.Second
			case "minute":
				unit = time.Minute
			case "hour":
				unit = time.Hour
			}
			return now.Add(time.Duration(n) * unit)
		}
	}
	return now.Truncate(time.Hour).Add(time.Hour)
}

// backoffDelay is 1s * 2^(consecutiveFailures-1), capped at maxDelay
// (spec: "exponential backoff, capped at refresh_interval"). A
// non-positive maxDelay (refresh_interval not yet configured) falls
// back to a 1h ceiling so backoff still terminates.
func backoffDelay(consecutiveFailures int, maxDelay time.Duration) time.Duration {
	if maxDelay <= 0 {
		maxDelay = time.Hour
	}
	d := time.Second
	for i := 1; i < consecutiveFailures && d < maxDelay; i++ {
		d *= 2
	}
	if d > maxDelay {
		d = maxDelay
	}
	return d
}

// probeRow is one parsed row of `op service-account ratelimit` output
// (columns TYPE ACTION LIMIT USED REMAINING RESET; RESET is ignored —
// the engine only needs LIMIT/USED for the ceiling and mismatch
// checks).
type probeRow struct {
	limit, used int
}

// parseProbeOutput parses the fixed-width `op service-account
// ratelimit` table into rows keyed "type/action" (e.g.
// "account/read_write"). Any row that doesn't parse as
// TYPE ACTION LIMIT USED REMAINING ... is skipped (covers the header
// row and any trailing blank lines); zero parseable rows is an error.
func parseProbeOutput(out []byte) (map[string]probeRow, error) {
	rows := map[string]probeRow{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || strings.EqualFold(fields[0], "TYPE") {
			continue
		}
		limit, err1 := strconv.Atoi(fields[2])
		used, err2 := strconv.Atoi(fields[3])
		if err1 != nil || err2 != nil {
			continue
		}
		rows[fields[0]+"/"+fields[1]] = probeRow{limit: limit, used: used}
	}
	if len(rows) == 0 {
		return nil, errors.New("op: ratelimit probe: no parseable rows in output")
	}
	return rows, nil
}

// probeNow execs ratelimit_probe_cmd (if configured) as
// `<cmd> service-account ratelimit` with OP_SERVICE_ACCOUNT_TOKEN set
// in the child process environment only — never argv, so the token
// never appears in a process listing (security requirement, spec
// D12). Any failure (missing binary, nonzero exit, timeout, parse
// error, missing account/read_write row) is fail-safe: it's recorded
// for op/status and the probe simply stops contributing to usagePct
// until a later successful run — it never blocks or fails cycle/read
// operations.
func (g *governor) probeNow(ctx context.Context) {
	g.mu.Lock()
	cmd := g.probeCmd
	token := g.token
	g.mu.Unlock()
	if cmd == "" {
		return
	}

	execCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	c := exec.CommandContext(execCtx, cmd, "service-account", "ratelimit")
	c.Env = append(os.Environ(), "OP_SERVICE_ACCOUNT_TOKEN="+token)
	out, err := c.Output()

	g.mu.Lock()
	defer g.mu.Unlock()
	g.lastProbeAt = g.now()

	if err != nil {
		g.probeHealthy = false
		g.probeErr = err.Error()
		g.probeWarning = ""
		return
	}
	rows, perr := parseProbeOutput(out)
	if perr != nil {
		g.probeHealthy = false
		g.probeErr = perr.Error()
		g.probeWarning = ""
		return
	}
	acct, ok := rows["account/read_write"]
	if !ok {
		g.probeHealthy = false
		g.probeErr = "op: ratelimit probe: missing account/read_write row"
		g.probeWarning = ""
		return
	}

	g.probeHealthy = true
	g.probeErr = ""
	if acct.limit > 0 {
		g.probedAccountPct = float64(acct.used) / float64(acct.limit) * 100
	}

	// Tighten-only semantics (spec §4/D12): probed *limit* values only
	// validate the configured numbers (mismatch -> status warning),
	// never override them.
	var warnings []string
	if tr, ok := rows["token/read"]; ok && g.hourlyLimit > 0 && tr.limit != g.hourlyLimit {
		warnings = append(warnings, fmt.Sprintf("probed token/read limit %d != configured hourly_read_limit %d", tr.limit, g.hourlyLimit))
	}
	if acct.limit > 0 && g.dailyLimit > 0 && acct.limit != g.dailyLimit {
		warnings = append(warnings, fmt.Sprintf("probed account/read_write limit %d != configured daily_request_limit %d", acct.limit, g.dailyLimit))
	}
	g.probeWarning = strings.Join(warnings, "; ")
}

// runProbeIfDue self-throttles probeNow to refreshInterval, the same
// cadence the delta cycle runs at — no separate probe-interval config
// knob is introduced. Intended to be called from PeriodicFunc
// (Task 7) on every tick, like the cycle's own self-gating.
func (g *governor) runProbeIfDue(ctx context.Context) {
	g.mu.Lock()
	cmd := g.probeCmd
	interval := g.refreshInterval
	due := cmd != "" && g.now().Sub(g.lastProbeAt) >= interval
	g.mu.Unlock()
	if !due {
		return
	}
	g.probeNow(ctx)
}
