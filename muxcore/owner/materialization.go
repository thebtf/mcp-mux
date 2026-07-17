package owner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/classify"
	"github.com/thebtf/mcp-mux/muxcore/internal/envidentity"
	"github.com/thebtf/mcp-mux/muxcore/jsonrpc"
	"github.com/thebtf/mcp-mux/muxcore/listchanged"
	"github.com/thebtf/mcp-mux/muxcore/remap"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
	"github.com/thebtf/mcp-mux/muxcore/upstream"
)

const (
	materializationFinalizeTimeout = 15 * time.Second
	materializationRetryInitial    = 100 * time.Millisecond
	materializationRetryMax        = 2 * time.Second
)

const restartMaterializationSettleGrace = 500 * time.Millisecond

var (
	materializationReadinessTimeout = 30 * time.Second
	materializationDemandTimeout    = 30 * time.Second
)

// MaterializationPolicy controls when an owner without a live upstream creates
// one. The zero value preserves the historical eager behavior.
type MaterializationPolicy uint8

const (
	MaterializationEager MaterializationPolicy = iota
	MaterializationOnDemand
	MaterializationPersistent
)

func (p MaterializationPolicy) String() string {
	switch p {
	case MaterializationOnDemand:
		return "on_demand"
	case MaterializationPersistent:
		return "persistent"
	default:
		return "eager"
	}
}

// MaterializationState is the owner-local process authority state.
type MaterializationState string

const (
	MaterializationCacheOnly       MaterializationState = "CACHE_ONLY"
	MaterializationMaterializing   MaterializationState = "MATERIALIZING"
	MaterializationReady           MaterializationState = "READY"
	MaterializationFinalizing      MaterializationState = "FINALIZING"
	MaterializationFinalizeBlocked MaterializationState = "FINALIZE_BLOCKED"
)

// MaterializationTrigger records why the current generation was requested.
type MaterializationTrigger string

const (
	MaterializationTriggerEager        MaterializationTrigger = "eager"
	MaterializationTriggerDemand       MaterializationTrigger = "demand"
	MaterializationTriggerBackground   MaterializationTrigger = "background"
	MaterializationTriggerUpstreamExit MaterializationTrigger = "upstream_exit"
	MaterializationTriggerWriteError   MaterializationTrigger = "write_error"
	MaterializationTriggerPersistent   MaterializationTrigger = "persistent"
)

var errFinalizationUnproven = errors.New("upstream process-tree finalization unproven")

type materializationSignals struct {
	initReady     chan struct{}
	initReadyOnce sync.Once
	initOK        bool

	discoveryReady     chan struct{}
	discoveryReadyOnce sync.Once

	failed     chan struct{}
	failedOnce sync.Once
	failureErr error
}

func newMaterializationSignals() *materializationSignals {
	return &materializationSignals{
		initReady:      make(chan struct{}),
		discoveryReady: make(chan struct{}),
		failed:         make(chan struct{}),
	}
}

type materializationAttempt struct {
	generation   uint64
	trigger      MaterializationTrigger
	launch       LaunchContext
	cancel       chan struct{}
	cancelOnce   sync.Once
	launchFrozen bool

	started     chan struct{}
	startedOnce sync.Once
	startedErr  error

	done     chan struct{}
	doneOnce sync.Once
	err      error

	process *upstream.Process
	signals *materializationSignals
}

func newMaterializationAttempt(generation uint64, trigger MaterializationTrigger) *materializationAttempt {
	return &materializationAttempt{
		generation: generation,
		trigger:    trigger,
		started:    make(chan struct{}),
		cancel:     make(chan struct{}),
		done:       make(chan struct{}),
		signals:    newMaterializationSignals(),
	}
}

func completedMaterializationAttempt(err error) *materializationAttempt {
	a := newMaterializationAttempt(0, "")
	a.startedErr = err
	a.err = err
	close(a.started)
	close(a.done)
	return a
}

func (a *materializationAttempt) signalStarted(err error) {
	a.startedOnce.Do(func() {
		a.startedErr = err
		close(a.started)
	})
}

func (s *materializationSignals) signalInit(ok bool) {
	s.initReadyOnce.Do(func() {
		s.initOK = ok
		close(s.initReady)
	})
}

func (s *materializationSignals) signalDiscoveryReady() {
	s.discoveryReadyOnce.Do(func() { close(s.discoveryReady) })
}

func (s *materializationSignals) signalFailure(err error) {
	if err == nil {
		err = errors.New("upstream materialization failed")
	}
	s.failedOnce.Do(func() {
		s.failureErr = err
		close(s.failed)
	})
}

func (a *materializationAttempt) finish(err error) {
	a.doneOnce.Do(func() {
		a.err = err
		close(a.done)
	})
}

func (a *materializationAttempt) cancelMaterialization() {
	a.cancelOnce.Do(func() { close(a.cancel) })
}

func (a *materializationAttempt) cancelled() bool {
	select {
	case <-a.cancel:
		return true
	default:
		return false
	}
}

// LaunchContext is the immutable effective process context selected for one
// materialization generation. Env is deep-copied when the context is elected.
type LaunchContext struct {
	SessionID int
	Cwd       string
	Env       map[string]string
}

type localDemandState string

const (
	localDemandWaiting    localDemandState = "WAITING"
	localDemandCancelled  localDemandState = "CANCELLED"
	localDemandTimedOut   localDemandState = "TIMED_OUT"
	localDemandFailed     localDemandState = "FAILED"
	localDemandRejected   localDemandState = "REJECTED"
	localDemandForwarding localDemandState = "FORWARDING"
)

type localDemand struct {
	key        string
	session    *Session
	message    *jsonrpc.Message
	timer      *time.Timer
	state      localDemandState
	generation uint64
}

type cacheStage struct {
	generation uint64
	process    *upstream.Process
	signals    *materializationSignals

	initResp             []byte
	toolList             []byte
	promptList           []byte
	resourceList         []byte
	resourceTemplateList []byte
	listChanged          bool
}

func (o *Owner) startMaterialization(trigger MaterializationTrigger) *materializationAttempt {
	o.materializationMu.Lock()
	a, start := o.startMaterializationLocked(trigger)
	o.materializationMu.Unlock()
	if start {
		go o.runMaterialization(a)
	}
	return a
}

func (o *Owner) startMaterializationLocked(trigger MaterializationTrigger) (*materializationAttempt, bool) {
	if o.upstreamWriter != nil || (o.sessionHandler != nil && o.handlerFunc == nil) {
		return completedMaterializationAttempt(nil), false
	}
	if o.restartPins.Load() > 0 {
		return completedMaterializationAttempt(errors.New("restart snapshot in progress")), false
	}
	select {
	case <-o.materializationStop:
		return completedMaterializationAttempt(errors.New("owner shutting down")), false
	default:
	}
	if o.materializationState == MaterializationFinalizeBlocked {
		err := o.materializationBlockedErr
		if err == nil {
			err = errFinalizationUnproven
		}
		return completedMaterializationAttempt(err), false
	}
	if o.materializationAttempt != nil {
		return o.materializationAttempt, false
	}
	if o.materializationState == MaterializationReady && !o.upstreamDead.Load() {
		return completedMaterializationAttempt(nil), false
	}

	o.materializationGeneration++
	a := newMaterializationAttempt(o.materializationGeneration, trigger)
	a.launch = o.electLaunchContextLocked()
	o.materializationAttempt = a
	o.materializationState = MaterializationMaterializing
	o.materializationTrigger = trigger
	return a, true
}

func (o *Owner) electLaunchContextLocked() LaunchContext {
	for _, key := range o.pendingDemandOrder {
		demand := o.pendingDemands[key]
		if demand != nil && demand.state == localDemandWaiting && demand.session != nil {
			return o.launchContextForSession(demand.session)
		}
	}
	return o.launchContextForSession(nil)
}

func (o *Owner) launchContextForSession(session *Session) LaunchContext {
	o.mu.RLock()
	cwd := o.cwd
	env := cloneLaunchEnv(o.env)
	o.mu.RUnlock()
	if session == nil {
		return LaunchContext{Cwd: cwd, Env: env}
	}
	if session.Cwd != "" {
		cwd = session.Cwd
	}
	if session.Env != nil {
		env = cloneLaunchEnv(session.Env)
	}
	return LaunchContext{SessionID: session.ID, Cwd: cwd, Env: env}
}

func cloneLaunchEnv(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func (o *Owner) launchContextCompatible(a, b LaunchContext) bool {
	aIdentity, bIdentity := envidentity.Build(a.Env), envidentity.Build(b.Env)
	if !envidentity.Equal(aIdentity, bIdentity, a.Env, b.Env) {
		return false
	}
	o.mu.RLock()
	mode := o.autoClassification
	o.mu.RUnlock()
	if mode == classify.ModeIsolated || mode == "" {
		return serverid.CanonicalizePath(a.Cwd) == serverid.CanonicalizePath(b.Cwd)
	}
	return true
}

var (
	errMaterializationCancelled     = errors.New("upstream materialization cancelled for restart")
	errMaterializationInitiatorGone = errors.New("materialization initiator disconnected before isolated classification commit")
)

func (o *Owner) runMaterialization(a *materializationAttempt) {
	wait := materializationRetryInitial
	attemptNumber := 0

	if current := o.currentUpstream(); current != nil {
		if err := o.retireMaterializationProcess(a, current); err != nil {
			a.signalStarted(err)
			o.finishMaterializationFailure(a, err)
			return
		}
	}

	for {
		attemptNumber++
		select {
		case <-o.materializationStop:
			err := errors.New("owner shutting down")
			a.signalStarted(err)
			o.finishMaterializationFailure(a, err)
			return
		case <-a.cancel:
			a.signalStarted(errMaterializationCancelled)
			o.finishMaterializationFailure(a, errMaterializationCancelled)
			return
		default:
		}

		if !o.materializationStillNeeded(a.trigger) {
			err := errors.New("upstream materialization no longer needed")
			a.signalStarted(err)
			o.finishMaterializationFailure(a, err)
			return
		}

		o.materializationMu.Lock()
		if !a.launchFrozen {
			a.launch = o.electLaunchContextLocked()
			a.launchFrozen = true
		}
		launch := LaunchContext{SessionID: a.launch.SessionID, Cwd: a.launch.Cwd, Env: cloneLaunchEnv(a.launch.Env)}
		o.materializationMu.Unlock()

		proc, err := o.spawnReplacementUpstream(launch)
		if err != nil {
			o.logger.Printf("upstream materialization start failed generation=%d attempt=%d trigger=%s err=%v", a.generation, attemptNumber, a.trigger, err)
			if a.cancelled() || !o.shouldRetryMaterialization(a.trigger) {
				a.signalStarted(err)
				o.finishMaterializationFailure(a, err)
				return
			}
			if !o.waitForMaterializationRetry(a, wait) {
				err = errMaterializationCancelled
				a.signalStarted(err)
				o.finishMaterializationFailure(a, err)
				return
			}
			wait = nextMaterializationRetry(wait)
			continue
		}

		signals := o.installMaterializationProcess(a, proc)
		a.signalStarted(nil)
		o.logger.Printf("upstream materialization started generation=%d attempt=%d trigger=%s", a.generation, attemptNumber, a.trigger)
		if a.cancelled() {
			if retireErr := o.retireMaterializationProcess(a, proc); retireErr != nil {
				o.finishMaterializationFailure(a, errors.Join(errMaterializationCancelled, retireErr))
			} else {
				o.finishMaterializationFailure(a, errMaterializationCancelled)
			}
			return
		}

		go o.readUpstream(proc)
		go o.monitorUpstreamExit(proc)
		if err := o.sendProactiveInitFor(a, signals); err != nil {
			signals.signalFailure(err)
		}

		timer := time.NewTimer(materializationReadinessTimeout)
		var readyErr error
		select {
		case <-signals.discoveryReady:
			select {
			case <-signals.failed:
				readyErr = signals.failureErr
			default:
			}
			if readyErr == nil && (o.upstreamDead.Load() || !o.isCurrentUpstream(proc)) {
				readyErr = errors.New("upstream exited before materialization completed")
			}
		case <-signals.failed:
			readyErr = signals.failureErr
		case <-timer.C:
			readyErr = fmt.Errorf("upstream materialization readiness timeout after %s", materializationReadinessTimeout)
		case <-a.cancel:
			readyErr = errMaterializationCancelled
		case <-o.materializationStop:
			readyErr = errors.New("owner shutting down")
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}

		if readyErr == nil {
			o.finishMaterializationSuccess(a)
			return
		}

		o.logger.Printf("upstream materialization failed generation=%d attempt=%d trigger=%s err=%v", a.generation, attemptNumber, a.trigger, readyErr)
		if err := o.retireMaterializationProcess(a, proc); err != nil {
			readyErr = errors.Join(readyErr, err)
			o.finishMaterializationFailure(a, readyErr)
			return
		}
		if errors.Is(readyErr, errMaterializationInitiatorGone) || a.cancelled() || !o.shouldRetryMaterialization(a.trigger) {
			o.finishMaterializationFailure(a, readyErr)
			return
		}
		if !o.waitForMaterializationRetry(a, wait) {
			o.finishMaterializationFailure(a, errMaterializationCancelled)
			return
		}
		wait = nextMaterializationRetry(wait)
		o.materializationMu.Lock()
		if o.materializationAttempt == a {
			o.materializationState = MaterializationMaterializing
		}
		o.materializationMu.Unlock()
	}
}

func nextMaterializationRetry(wait time.Duration) time.Duration {
	wait *= 2
	if wait > materializationRetryMax {
		return materializationRetryMax
	}
	return wait
}

func (o *Owner) materializationStillNeeded(trigger MaterializationTrigger) bool {
	if trigger == MaterializationTriggerEager || trigger == MaterializationTriggerBackground || trigger == MaterializationTriggerPersistent {
		return true
	}
	o.materializationMu.Lock()
	pending := len(o.pendingDemands)
	persistenceObligation := o.materializationPolicy == MaterializationPersistent || o.persistentPending
	o.materializationMu.Unlock()
	return pending > 0 || persistenceObligation || o.SessionCount() > 0
}

func (o *Owner) shouldRetryMaterialization(trigger MaterializationTrigger) bool {
	select {
	case <-o.materializationStop:
		return false
	default:
	}
	o.materializationMu.Lock()
	pending := len(o.pendingDemands)
	persistenceObligation := o.materializationPolicy == MaterializationPersistent || o.persistentPending
	o.materializationMu.Unlock()
	if persistenceObligation || pending > 0 {
		return true
	}
	return (trigger == MaterializationTriggerUpstreamExit || trigger == MaterializationTriggerWriteError) && o.SessionCount() > 0
}

func (o *Owner) waitForMaterializationRetry(a *materializationAttempt, wait time.Duration) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-a.cancel:
		return false
	case <-o.materializationStop:
		return false
	}
}

func (o *Owner) installMaterializationProcess(a *materializationAttempt, proc *upstream.Process) *materializationSignals {
	signals := newMaterializationSignals()
	o.upstreamEventMu.Lock()
	o.materializationMu.Lock()
	a.process = proc
	a.signals = signals
	o.cacheStage = &cacheStage{generation: a.generation, process: proc, signals: signals}
	o.mu.Lock()
	o.upstream = proc
	o.upstreamDead.Store(false)
	o.mu.Unlock()
	o.materializationMu.Unlock()
	o.upstreamEventMu.Unlock()
	return signals
}

func (o *Owner) retireMaterializationProcess(a *materializationAttempt, proc *upstream.Process) error {
	if proc == nil {
		return nil
	}
	o.upstreamEventMu.Lock()
	o.materializationMu.Lock()
	if o.materializationAttempt == a {
		o.materializationState = MaterializationFinalizing
		o.retiringProcess = proc
	}
	o.materializationMu.Unlock()
	o.upstreamEventMu.Unlock()

	closeErr := proc.Close()
	if closeErr != nil {
		o.logger.Printf("upstream finalizer failed generation=%d: %v", a.generation, closeErr)
	}
	var finalizationErr error
	if o.materializationFinalizationProbe != nil {
		finalizationErr = o.materializationFinalizationProbe(proc)
	} else if !proc.RetirementProven() {
		timer := time.NewTimer(materializationFinalizeTimeout)
		defer timer.Stop()
		select {
		case <-proc.Done:
		case <-timer.C:
			finalizationErr = fmt.Errorf("%w after %s", errFinalizationUnproven, materializationFinalizeTimeout)
		case <-o.materializationStop:
			select {
			case <-proc.Done:
			case <-time.After(materializationFinalizeTimeout):
				finalizationErr = fmt.Errorf("%w during shutdown", errFinalizationUnproven)
			}
		}
		if finalizationErr == nil && !proc.RetirementProven() {
			finalizationErr = errFinalizationUnproven
		}
	}
	if finalizationErr != nil {
		if closeErr != nil {
			finalizationErr = errors.Join(finalizationErr, closeErr)
		}
		return o.blockMaterializationFinalization(a, finalizationErr)
	}

	o.upstreamEventMu.Lock()
	o.materializationMu.Lock()
	o.mu.Lock()
	if o.upstream == proc {
		o.upstream = nil
	}
	o.upstreamDead.Store(true)
	o.mu.Unlock()
	if o.retiringProcess == proc {
		o.retiringProcess = nil
	}
	if o.cacheStage != nil && o.cacheStage.process == proc {
		o.cacheStage = nil
	}
	if o.materializationAttempt == a && o.materializationState == MaterializationFinalizing {
		o.materializationState = MaterializationMaterializing
	}
	o.materializationMu.Unlock()
	o.upstreamEventMu.Unlock()
	o.clearProactiveTracking()
	return nil
}

func (o *Owner) blockMaterializationFinalization(a *materializationAttempt, err error) error {
	o.upstreamEventMu.Lock()
	o.materializationMu.Lock()
	if o.materializationAttempt == a {
		o.materializationState = MaterializationFinalizeBlocked
		o.materializationBlockedErr = err
	}
	o.materializationMu.Unlock()
	o.upstreamEventMu.Unlock()
	return err
}

func (o *Owner) finishMaterializationSuccess(a *materializationAttempt) {
	for {
		o.materializationMu.Lock()
		if o.materializationAttempt != a {
			o.materializationMu.Unlock()
			a.finish(errors.New("materialization attempt superseded"))
			return
		}
		order := append([]string(nil), o.pendingDemandOrder...)
		o.pendingDemandOrder = nil
		if len(order) == 0 && len(o.pendingDemands) == 0 {
			o.materializationAttempt = nil
			o.materializationState = MaterializationReady
			o.materializationTrigger = a.trigger
			o.restartResumeTrigger = ""
			o.materializationMu.Unlock()
			a.finish(nil)
			o.logger.Printf("upstream materialization ready generation=%d trigger=%s", a.generation, a.trigger)
			return
		}
		o.materializationMu.Unlock()

		for _, key := range order {
			o.forwardQueuedDemand(key, a.generation, a.launch)
		}
	}
}

func (o *Owner) finishMaterializationFailure(a *materializationAttempt, err error) {
	a.signalStarted(err)
	o.clearProactiveTracking()
	o.materializationMu.Lock()
	demands := o.detachLocalDemandsForGenerationLocked(a.generation)
	if o.materializationAttempt == a {
		o.materializationAttempt = nil
		if o.materializationState != MaterializationFinalizeBlocked {
			o.materializationState = MaterializationCacheOnly
		}
		o.cacheStage = nil
	}
	o.materializationMu.Unlock()
	a.finish(err)
	o.writeFailedLocalDemands(demands, err)
	o.resumeMaterializationAfterRestartPin()
}

func (o *Owner) clearProactiveTracking() {
	o.methodTags.Range(func(key, _ any) bool {
		id, ok := key.(string)
		if !ok || !o.isProactiveID(json.RawMessage(id)) {
			return true
		}
		if _, loaded := o.methodTags.LoadAndDelete(id); loaded {
			o.decrementPending()
		}
		return true
	})
}

func (o *Owner) currentUpstream() *upstream.Process {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.upstream
}

func (o *Owner) acceptsProcessEvent(proc *upstream.Process) bool {
	if proc == nil {
		return true
	}
	o.materializationMu.Lock()
	blocked := o.retiringProcess == proc || o.materializationState == MaterializationFinalizing || o.materializationState == MaterializationFinalizeBlocked
	o.mu.RLock()
	current := o.upstream
	o.mu.RUnlock()
	o.materializationMu.Unlock()
	return !blocked && current == proc && !o.upstreamDead.Load()
}

func (o *Owner) sendProactiveInitFor(a *materializationAttempt, signals *materializationSignals) error {
	proc := a.process
	if proc == nil {
		return errors.New("upstream process unavailable")
	}
	if a.cancelled() {
		return errMaterializationCancelled
	}
	initKey := strconv.Quote(fmt.Sprintf("%s-%d-0", o.proactiveNamespace, a.generation))
	initReq := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"roots":{"listChanged":true}},"clientInfo":{"name":"mcp-mux","version":"1.0.0"}}}`, initKey))
	o.methodTags.Store(initKey, "initialize")
	o.pendingRequests.Add(1)
	if err := proc.WriteLine(initReq); err != nil {
		return fmt.Errorf("proactive initialize write: %w", err)
	}
	o.logger.Printf("proactive init: sent initialize request generation=%d", a.generation)

	timer := time.NewTimer(materializationReadinessTimeout)
	defer timer.Stop()
	select {
	case <-signals.initReady:
		if !signals.initOK {
			return errors.New("proactive initialize did not produce a usable response")
		}
	case <-signals.failed:
		return signals.failureErr
	case <-timer.C:
		return fmt.Errorf("proactive initialize timeout after %s", materializationReadinessTimeout)
	case <-a.cancel:
		return errMaterializationCancelled
	case <-o.materializationStop:
		return errors.New("owner shutting down")
	}

	if a.cancelled() {
		return errMaterializationCancelled
	}
	if err := proc.WriteLine([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); err != nil {
		return fmt.Errorf("proactive initialized notification: %w", err)
	}
	toolsKey := strconv.Quote(fmt.Sprintf("%s-%d-1", o.proactiveNamespace, a.generation))
	toolsReq := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"tools/list","params":{}}`, toolsKey))
	o.methodTags.Store(toolsKey, "tools/list")
	o.pendingRequests.Add(1)
	if err := proc.WriteLine(toolsReq); err != nil {
		return fmt.Errorf("proactive tools/list write: %w", err)
	}
	return nil
}

func (o *Owner) cacheResponseFrom(proc *upstream.Process, method string, raw []byte) {
	if proc == nil {
		o.cacheResponse(method, raw)
		return
	}
	o.upstreamEventMu.RLock()
	defer o.upstreamEventMu.RUnlock()
	if !o.acceptsProcessEvent(proc) {
		return
	}
	o.cacheResponseFromLocked(proc, method, raw)
}

// cacheResponseFromLocked is called only with the exact generation's read
// lease held. It avoids recursively acquiring RWMutex.RLock from the message
// handler, which would deadlock behind a queued writer.
func (o *Owner) cacheResponseFromLocked(proc *upstream.Process, method string, raw []byte) {
	// Active materialization stages are generation-fenced by attempt/process CAS.
	if proc != nil && o.stageMaterializationCacheResponse(proc, method, raw) {
		return
	}
	if o.cacheResponseState(method, raw) {
		o.broadcastListChanged()
	}
}

func (o *Owner) stageMaterializationListChanged(proc *upstream.Process, method string) bool {
	switch method {
	case "notifications/tools/list_changed", "notifications/prompts/list_changed", "notifications/resources/list_changed":
	default:
		return false
	}
	o.materializationMu.Lock()
	defer o.materializationMu.Unlock()
	a := o.materializationAttempt
	stage := o.cacheStage
	if a == nil || stage == nil || stage.process != proc || a.process != proc {
		return false
	}
	stage.listChanged = true
	return true
}

func (o *Owner) stageMaterializationCacheResponse(proc *upstream.Process, method string, raw []byte) bool {
	cached := append([]byte(nil), raw...)
	if method == "initialize" {
		if injected, ok := listchanged.InjectInitializeCapability(cached); ok {
			cached = injected
		}
	}

	o.materializationMu.Lock()
	a := o.materializationAttempt
	stage := o.cacheStage
	if a == nil || stage == nil || stage.process != proc || a.process != proc {
		o.materializationMu.Unlock()
		return false
	}
	persistentDetected := false
	switch method {
	case "initialize":
		stage.initResp = cached
		if classify.ParsePersistent(cached) && !o.persistentPending {
			o.persistentPending = true
			o.materializationPolicy = MaterializationPersistent
			persistentDetected = true
		}
	case "tools/list":
		stage.toolList = cached
	case "prompts/list":
		stage.promptList = cached
	case "resources/list":
		stage.resourceList = cached
	case "resources/templates/list":
		stage.resourceTemplateList = cached
	default:
		o.materializationMu.Unlock()
		return false
	}
	readyToCommit := stage.initResp != nil && stage.toolList != nil
	if method == "initialize" {
		stage.signals.signalInit(true)
	}
	o.materializationMu.Unlock()

	if persistentDetected && o.onPersistentDetected != nil {
		o.onPersistentDetected(o)
	}
	if readyToCommit {
		o.commitMaterializationCache(a)
	}
	return true
}

func (o *Owner) commitMaterializationCache(a *materializationAttempt) {
	o.materializationMu.Lock()
	if o.materializationAttempt != a || o.cacheStage == nil || o.cacheStage.generation != a.generation {
		o.materializationMu.Unlock()
		return
	}
	o.admissionMu.Lock()
	stage := o.cacheStage
	freshMode, freshSource, freshReason := stagedClassification(stage.initResp, stage.toolList)
	persistent := classify.ParsePersistent(stage.initResp)

	o.mu.Lock()
	if freshMode == classify.ModeIsolated && a.launch.SessionID != 0 {
		if _, initiatorPresent := o.sessions[a.launch.SessionID]; !initiatorPresent {
			o.mu.Unlock()
			o.admissionMu.Unlock()
			o.materializationMu.Unlock()
			stage.signals.signalFailure(errMaterializationInitiatorGone)
			return
		}
	}
	hadCachedLists := o.toolList != nil || o.promptList != nil || o.resourceList != nil || o.resourceTemplateList != nil
	stagedSnapshot := o.stagedSnapshotLocked(a, stage, freshMode, freshSource, freshReason, persistent)
	liveMode, liveSource := freshMode, freshSource
	liveReason := append([]string(nil), freshReason...)
	if o.autoClassification == classify.ModeIsolated && freshMode != classify.ModeIsolated {
		liveMode = classify.ModeIsolated
		liveSource = o.classificationSource
		liveReason = append([]string(nil), o.classificationReason...)
	}
	o.admissionFrozen.Store(true)
	o.mu.Unlock()

	// The daemon's compare-and-publish is the generation commit point. The
	// staged snapshot intentionally carries fresh classification for future
	// templates, while local liveMode may remain conservatively isolated.
	if o.onCacheReady != nil && !o.onCacheReady(o, stagedSnapshot) {
		o.admissionFrozen.Store(false)
		o.admissionMu.Unlock()
		o.materializationMu.Unlock()
		stage.signals.signalFailure(errors.New("stale owner generation rejected cache commit"))
		return
	}

	o.mu.Lock()
	o.initResp = append([]byte(nil), stage.initResp...)
	o.toolList = append([]byte(nil), stage.toolList...)
	o.promptList = append([]byte(nil), stage.promptList...)
	o.resourceList = append([]byte(nil), stage.resourceList...)
	o.resourceTemplateList = append([]byte(nil), stage.resourceTemplateList...)
	o.initDone = true
	o.autoClassification = liveMode
	o.classificationSource = liveSource
	o.classificationReason = liveReason
	o.cwd = a.launch.Cwd
	o.env = cloneLaunchEnv(a.launch.Env)
	evictees := o.isolatedEvicteesLocked(liveMode, a.launch.SessionID)
	o.mu.Unlock()

	o.persistentPending = false
	if persistent || o.persistentRequired {
		o.materializationPolicy = MaterializationPersistent
	} else if o.materializationPolicy == MaterializationPersistent {
		o.materializationPolicy = MaterializationOnDemand
	}

	var rejected []*localDemand
	if len(evictees) > 0 {
		evictedIDs := make(map[int]struct{}, len(evictees))
		for _, s := range evictees {
			evictedIDs[s.ID] = struct{}{}
		}
		for key, demand := range o.pendingDemands {
			if _, evicted := evictedIDs[demand.session.ID]; evicted && demand.state == localDemandWaiting {
				demand.state = localDemandRejected
				delete(o.pendingDemands, key)
				rejected = append(rejected, demand)
			}
		}
		if len(rejected) > 0 {
			order := o.pendingDemandOrder[:0]
			for _, key := range o.pendingDemandOrder {
				if _, exists := o.pendingDemands[key]; exists {
					order = append(order, key)
				}
			}
			o.pendingDemandOrder = order
		}
	}
	shouldBroadcast := hadCachedLists || stage.listChanged
	o.cacheStage = nil
	o.admissionFrozen.Store(false)
	o.admissionMu.Unlock()
	o.materializationMu.Unlock()

	o.initReadyOnce.Do(func() {
		if o.initReady != nil {
			close(o.initReady)
		}
	})
	for _, demand := range rejected {
		if demand.timer != nil {
			demand.timer.Stop()
		}
	}
	for _, s := range evictees {
		s.Close()
	}
	o.classifiedOnce.Do(func() {
		if o.classified != nil {
			close(o.classified)
		}
	})
	if o.onPersistentResolved != nil {
		o.onPersistentResolved(o, persistent)
	}
	o.parseDrainTimeout(stage.initResp)
	o.parseToolTimeout(stage.initResp)
	o.parseIdleTimeout(stage.initResp)
	if sec := classify.ParseProgressInterval(stage.initResp); sec > 0 {
		o.progressIntervalNs.Store(int64(time.Duration(sec) * time.Second))
	}
	if shouldBroadcast {
		o.broadcastListChanged()
	}
	stage.signals.signalDiscoveryReady()
}

func (o *Owner) stagedSnapshotLocked(a *materializationAttempt, stage *cacheStage, mode classify.SharingMode, source string, reason []string, persistent bool) OwnerSnapshot {
	snap := o.exportSnapshotLocked()
	snap.Cwd = a.launch.Cwd
	snap.Env = cloneSnapshotEnv(a.launch.Env)
	snap.Classification = mode
	snap.ClassificationSource = source
	snap.ClassificationReason = append([]string(nil), reason...)
	snap.CachedInit = base64Encode(stage.initResp)
	snap.CachedTools = base64Encode(stage.toolList)
	snap.CachedPrompts = base64Encode(stage.promptList)
	snap.CachedResources = base64Encode(stage.resourceList)
	snap.CachedResourceTemplates = base64Encode(stage.resourceTemplateList)
	snap.Persistent = persistent || o.persistentRequired
	primary := serverid.CanonicalizePath(a.launch.Cwd)
	if mode == classify.ModeIsolated {
		snap.CwdSet = []string{primary}
	} else if primary != "" {
		seen := false
		for _, cwd := range snap.CwdSet {
			if serverid.CanonicalizePath(cwd) == primary {
				seen = true
				break
			}
		}
		if !seen {
			snap.CwdSet = append(snap.CwdSet, primary)
		}
	}
	return snap
}

func stagedClassification(initJSON, toolsJSON []byte) (classify.SharingMode, string, []string) {
	if mode, ok := classify.ClassifyCapabilities(initJSON); ok {
		return mode, "capability", nil
	}
	mode, matched := classify.ClassifyTools(toolsJSON)
	return mode, "tools", matched
}

func (o *Owner) isolatedEvicteesLocked(mode classify.SharingMode, keepID int) []*Session {
	if mode != classify.ModeIsolated {
		return nil
	}
	primaryCwd := serverid.CanonicalizePath(o.cwd)
	o.cwdSet = map[string]bool{primaryCwd: true}
	o.sessionMgr.RemovePendingForOwnerExcept(o.serverID, o.initialAdmissionToken)
	if len(o.sessions) <= 1 {
		return nil
	}
	if _, ok := o.sessions[keepID]; !ok {
		keepID = -1
		for id := range o.sessions {
			if keepID == -1 || id < keepID {
				keepID = id
			}
		}
	}
	evictees := make([]*Session, 0, len(o.sessions)-1)
	for id, s := range o.sessions {
		if id != keepID {
			evictees = append(evictees, s)
		}
	}
	return evictees
}

func (o *Owner) enqueueLocalDemand(s *Session, msg *jsonrpc.Message) (bool, error) {
	raw := append([]byte(nil), msg.Raw...)
	copyMsg, err := jsonrpc.Parse(raw)
	if err != nil {
		return false, err
	}
	key := string(remap.Remap(s.ID, msg.ID))
	candidate := o.launchContextForSession(s)

	o.materializationMu.Lock()
	if o.materializationState == MaterializationReady && !o.upstreamDead.Load() {
		o.materializationMu.Unlock()
		return false, nil
	}
	if o.restartPins.Load() > 0 {
		o.materializationMu.Unlock()
		return true, o.writeDemandError(s, msg.ID, "restart snapshot in progress")
	}
	if o.materializationState == MaterializationFinalizeBlocked {
		err := o.materializationBlockedErr
		if err == nil {
			err = errFinalizationUnproven
		}
		o.materializationMu.Unlock()
		return true, o.writeDemandError(s, msg.ID, err.Error())
	}
	if _, exists := o.pendingDemands[key]; exists {
		o.materializationMu.Unlock()
		return true, o.writeDemandError(s, msg.ID, "duplicate request id while upstream materializes")
	}
	if active := o.materializationAttempt; active != nil && !o.launchContextCompatible(active.launch, candidate) {
		o.materializationMu.Unlock()
		return true, o.writeDemandError(s, msg.ID, "request context incompatible with active upstream materialization")
	}

	demand := &localDemand{key: key, session: s, message: copyMsg, state: localDemandWaiting}
	o.pendingDemands[key] = demand
	o.pendingDemandOrder = append(o.pendingDemandOrder, key)
	demand.timer = time.AfterFunc(materializationDemandTimeout, func() {
		o.expireLocalDemand(key)
	})
	a, start := o.startMaterializationLocked(MaterializationTriggerDemand)
	if a.generation == 0 {
		delete(o.pendingDemands, key)
		demand.state = localDemandFailed
		if demand.timer != nil {
			demand.timer.Stop()
		}
		o.materializationMu.Unlock()
		if a.err != nil {
			return true, o.writeDemandError(s, msg.ID, a.err.Error())
		}
		return false, nil
	}
	demand.generation = a.generation
	o.materializationMu.Unlock()
	if start {
		go o.runMaterialization(a)
	}
	return true, nil
}

func (o *Owner) expireLocalDemand(key string) {
	o.materializationMu.Lock()
	demand, ok := o.transitionLocalDemandLocked(key, localDemandTimedOut)
	o.materializationMu.Unlock()
	if ok {
		_ = o.writeDemandError(demand.session, demand.message.ID, "upstream materialization timed out")
	}
}

func (o *Owner) cancelLocalDemand(s *Session, requestID json.RawMessage) bool {
	key := string(remap.Remap(s.ID, requestID))
	if o.beforeLocalDemandCancel != nil {
		o.beforeLocalDemandCancel()
	}
	o.materializationMu.Lock()
	demand, ok := o.transitionLocalDemandLocked(key, localDemandCancelled)
	if ok && o.materializationAttempt != nil && !o.materializationAttempt.launchFrozen && demand.session.ID == o.materializationAttempt.launch.SessionID {
		o.materializationAttempt.launch = o.electLaunchContextLocked()
	}
	o.materializationMu.Unlock()
	return ok
}

func (o *Owner) cancelLocalDemandsForSession(sessionID int) {
	o.materializationMu.Lock()
	launchRemoved := false
	for key, demand := range o.pendingDemands {
		if demand.session.ID != sessionID || demand.state != localDemandWaiting {
			continue
		}
		demand.state = localDemandCancelled
		delete(o.pendingDemands, key)
		if demand.timer != nil {
			demand.timer.Stop()
		}
		if o.materializationAttempt != nil && demand.session.ID == o.materializationAttempt.launch.SessionID {
			launchRemoved = true
		}
	}
	if launchRemoved && o.materializationAttempt != nil && !o.materializationAttempt.launchFrozen {
		o.materializationAttempt.launch = o.electLaunchContextLocked()
	}
	o.materializationMu.Unlock()
}

func (o *Owner) cancelDisposableMaterializationForZeroSessions() {
	o.materializationMu.Lock()
	a := o.materializationAttempt
	if a == nil || o.materializationPolicy == MaterializationPersistent || o.persistentPending || o.restartPins.Load() > 0 || a.trigger == MaterializationTriggerBackground || a.trigger == MaterializationTriggerPersistent {
		o.materializationMu.Unlock()
		return
	}
	a.cancelMaterialization()
	o.materializationMu.Unlock()
}

func (o *Owner) detachLocalDemandsForGenerationLocked(generation uint64) []*localDemand {
	demands := make([]*localDemand, 0, len(o.pendingDemands))
	for key, demand := range o.pendingDemands {
		if demand.state != localDemandWaiting || generation != 0 && demand.generation != generation {
			continue
		}
		demand.state = localDemandFailed
		delete(o.pendingDemands, key)
		if demand.timer != nil {
			demand.timer.Stop()
		}
		demands = append(demands, demand)
	}
	kept := o.pendingDemandOrder[:0]
	for _, key := range o.pendingDemandOrder {
		if demand := o.pendingDemands[key]; demand != nil && demand.state == localDemandWaiting {
			kept = append(kept, key)
		}
	}
	o.pendingDemandOrder = kept
	return demands
}

func (o *Owner) detachAllLocalDemands() []*localDemand {
	o.materializationMu.Lock()
	demands := o.detachLocalDemandsForGenerationLocked(0)
	o.materializationMu.Unlock()
	return demands
}

func (o *Owner) writeFailedLocalDemands(demands []*localDemand, err error) {
	if err == nil {
		err = errors.New("upstream materialization failed")
	}
	for _, demand := range demands {
		_ = o.writeDemandError(demand.session, demand.message.ID, err.Error())
	}
}

func (o *Owner) failAllLocalDemands(err error) {
	o.writeFailedLocalDemands(o.detachAllLocalDemands(), err)
}

func (o *Owner) forwardQueuedDemand(key string, generation uint64, launch LaunchContext) {
	o.materializationMu.Lock()
	demand := o.pendingDemands[key]
	if demand == nil || demand.state != localDemandWaiting || demand.generation != generation {
		o.materializationMu.Unlock()
		return
	}

	o.mu.RLock()
	attached := o.sessions[demand.session.ID] == demand.session
	o.mu.RUnlock()
	authorized := o.authorizeSession == nil || demand.session.Meta().IsAuthorized()
	compatible := o.launchContextCompatible(launch, o.launchContextForSession(demand.session))
	if !attached || !authorized || !compatible {
		demand.state = localDemandRejected
		delete(o.pendingDemands, key)
		if demand.timer != nil {
			demand.timer.Stop()
		}
		o.materializationMu.Unlock()
		_ = o.writeDemandError(demand.session, demand.message.ID, "request no longer admitted after upstream materialization")
		return
	}

	demand.state = localDemandForwarding
	delete(o.pendingDemands, key)
	if demand.timer != nil {
		demand.timer.Stop()
	}
	if cached := o.getCachedResponse(demand.message.Method); cached != nil &&
		(demand.message.Method != "initialize" || o.initFingerprintMatches(demand.message.Raw)) {
		err := o.replayFromCache(demand.session, demand.message, cached)
		o.materializationMu.Unlock()
		if err != nil {
			_ = o.writeDemandError(demand.session, demand.message.ID, err.Error())
		}
		return
	}

	if o.beforeQueuedDemandWrite != nil {
		o.beforeQueuedDemandWrite()
	}
	writeFailed, err := o.forwardRequestPrepared(demand.session, demand.message)
	o.materializationMu.Unlock()
	if err != nil {
		o.logger.Printf("session %d: queued demand forwarding failed: %v", demand.session.ID, err)
	}
	if writeFailed {
		o.startMaterialization(MaterializationTriggerWriteError)
	}
}

func (o *Owner) transitionLocalDemandLocked(key string, terminal localDemandState) (*localDemand, bool) {
	demand := o.pendingDemands[key]
	if demand == nil || demand.state != localDemandWaiting {
		return nil, false
	}
	demand.state = terminal
	delete(o.pendingDemands, key)
	if demand.timer != nil {
		demand.timer.Stop()
	}
	return demand, true
}

func (o *Owner) writeDemandError(s *Session, id json.RawMessage, message string) error {
	payload, err := buildJSONRPCErrorBytes(id, -32603, message)
	if err != nil {
		return err
	}
	return s.WriteRaw(payload)
}

func (o *Owner) forwardRequestNow(s *Session, msg *jsonrpc.Message) error {
	writeFailed, err := o.forwardRequestPrepared(s, msg)
	if writeFailed {
		o.startMaterialization(MaterializationTriggerWriteError)
	}
	return err
}

func (o *Owner) forwardRequestPrepared(s *Session, msg *jsonrpc.Message) (bool, error) {
	o.pendingRequests.Add(1)
	newID := remap.Remap(s.ID, msg.ID)
	remapped, err := jsonrpc.ReplaceID(msg.Raw, newID)
	if err != nil {
		o.decrementPending()
		return false, fmt.Errorf("remap request: %w", err)
	}

	o.inflightTracker.Store(string(newID), &InflightRequest{
		Method:    msg.Method,
		Tool:      extractToolName(msg.Raw),
		SessionID: s.ID,
		StartTime: time.Now(),
	})
	if msg.Method == "initialize" || msg.Method == "tools/list" ||
		msg.Method == "prompts/list" || msg.Method == "resources/list" ||
		msg.Method == "resources/templates/list" {
		o.methodTags.Store(string(newID), msg.Method)
	}
	if msg.Method == "initialize" {
		o.captureInitFingerprint(msg.Raw)
	}

	o.mu.RLock()
	needsMeta := o.autoClassification == classify.ModeSessionAware || o.handlerFunc != nil
	o.mu.RUnlock()
	if needsMeta {
		if injected, injectErr := jsonrpc.InjectMeta(remapped, "muxSessionId", s.MuxSessionID); injectErr == nil {
			remapped = injected
		} else {
			o.logger.Printf("session %d: failed to inject muxSessionId: %v", s.ID, injectErr)
		}
		if s.Cwd != "" {
			if injected, injectErr := jsonrpc.InjectMeta(remapped, "muxCwd", s.Cwd); injectErr == nil {
				remapped = injected
			} else {
				o.logger.Printf("session %d: failed to inject muxCwd: %v", s.ID, injectErr)
			}
		}
		if len(s.Env) > 0 {
			if injected, injectErr := jsonrpc.InjectMeta(remapped, "muxEnv", s.Env); injectErr == nil {
				remapped = injected
			} else {
				o.logger.Printf("session %d: failed to inject muxEnv: %v", s.ID, injectErr)
			}
		}
	}

	o.trackProgressToken(s.ID, string(newID), msg.Raw)
	o.sessionMgr.TrackRequest(string(newID), s.ID)
	if msg.Method == "tools/call" && o.toolTimeoutNs.Load() > 0 {
		o.startToolWatchdog(string(newID), msg.ID, s, msg.Method)
	}
	if err := o.writeUpstream(remapped); err != nil {
		o.logger.Printf("session %d: upstream write failed for request id=%s: %v", s.ID, string(newID), err)
		o.upstreamDead.Store(true)
		o.drainInflightRequests()
		return true, nil
	}
	return false, nil
}

func (o *Owner) ensureUpstreamReadyForRequest() error {
	if o.materializationReadyForRequests() {
		return nil
	}
	a := o.startMaterialization(MaterializationTriggerDemand)
	timer := time.NewTimer(materializationReadinessTimeout)
	defer timer.Stop()
	select {
	case <-a.done:
		return a.err
	case <-timer.C:
		return fmt.Errorf("upstream materialization still pending after %s", materializationReadinessTimeout)
	case <-o.materializationStop:
		return errors.New("owner shutting down")
	}
}

func (o *Owner) materializationReadyForRequests() bool {
	if o.upstreamWriter != nil || (o.sessionHandler != nil && o.handlerFunc == nil) {
		return true
	}
	o.materializationMu.Lock()
	ready := o.materializationState == MaterializationReady && !o.upstreamDead.Load()
	o.materializationMu.Unlock()
	return ready && o.hasWritableUpstream()
}

func (o *Owner) forwardNotificationIfReady(data []byte) error {
	if !o.materializationReadyForRequests() {
		return nil
	}
	return o.writeUpstream(data)
}

// StartInitialMaterialization starts the daemon-deferred initial generation and
// waits until process start succeeds or fails. Discovery continues through the
// normal controller after this method returns.
func (o *Owner) StartInitialMaterialization() error {
	trigger := MaterializationTriggerEager
	o.materializationMu.Lock()
	if o.materializationPolicy == MaterializationPersistent || o.persistentPending {
		trigger = MaterializationTriggerPersistent
	}
	o.materializationMu.Unlock()
	a := o.startMaterialization(trigger)
	<-a.started
	return a.startedErr
}

// SpawnUpstreamBackground retains the existing call surface while routing the
// work through the single owner-owned materialization controller.
func (o *Owner) SpawnUpstreamBackground() {
	o.startMaterialization(MaterializationTriggerBackground)
}

func (o *Owner) monitorMaterializedProcessExit(proc *upstream.Process) {
	if proc == nil {
		return
	}
	o.upstreamEventMu.Lock()
	o.materializationMu.Lock()
	if o.retiringProcess == proc {
		o.materializationMu.Unlock()
		o.upstreamEventMu.Unlock()
		o.handleRetiringProcessExit(proc)
		return
	}
	o.mu.Lock()
	if o.upstream != proc {
		o.mu.Unlock()
		o.materializationMu.Unlock()
		o.upstreamEventMu.Unlock()
		return
	}
	o.upstreamDead.Store(true)
	if a := o.materializationAttempt; a != nil && a.process == proc {
		signals := a.signals
		o.mu.Unlock()
		responses := o.claimInflightRequests()
		o.materializationMu.Unlock()
		o.upstreamEventMu.Unlock()
		o.deliverInflightResponses(responses)
		signals.signalFailure(fmt.Errorf("upstream exited before materialization completed: %v", proc.ExitErr))
		return
	}
	o.upstream = nil
	o.mu.Unlock()
	o.materializationState = MaterializationCacheOnly
	responses := o.claimInflightRequests()
	o.materializationMu.Unlock()
	o.upstreamEventMu.Unlock()
	o.deliverInflightResponses(responses)

	if o.shouldRetryMaterialization(MaterializationTriggerUpstreamExit) {
		o.startMaterialization(MaterializationTriggerUpstreamExit)
		return
	}
	if !o.hasUsableCache() {
		o.notifyUpstreamExit()
	}
}

func (o *Owner) handleRetiringProcessExit(proc *upstream.Process) bool {
	o.upstreamEventMu.Lock()
	o.materializationMu.Lock()
	if o.retiringProcess != proc {
		o.materializationMu.Unlock()
		o.upstreamEventMu.Unlock()
		return false
	}
	if o.materializationState != MaterializationFinalizeBlocked || !errors.Is(o.materializationBlockedErr, errFinalizationUnproven) {
		o.materializationMu.Unlock()
		o.upstreamEventMu.Unlock()
		return true
	}
	o.materializationMu.Unlock()
	o.upstreamEventMu.Unlock()

	// Process finalization may block; generation writers already see this proc
	// as retiring, so no lease is held across Close.
	closeErr := proc.Close()
	proofErr := error(nil)
	proven := proc.RetirementProven()
	if o.materializationFinalizationProbe != nil {
		proofErr = o.materializationFinalizationProbe(proc)
		proven = proofErr == nil
	}
	if !proven {
		if proofErr == nil {
			proofErr = errFinalizationUnproven
		}
		if closeErr != nil {
			proofErr = errors.Join(proofErr, closeErr)
		}
		o.upstreamEventMu.Lock()
		o.materializationMu.Lock()
		if o.retiringProcess == proc {
			o.materializationBlockedErr = proofErr
		}
		o.materializationMu.Unlock()
		o.upstreamEventMu.Unlock()
		return true
	}
	if closeErr != nil {
		o.logger.Printf("retiring process finalized after warning: %v", closeErr)
	}

	o.upstreamEventMu.Lock()
	o.materializationMu.Lock()
	if o.retiringProcess != proc {
		o.materializationMu.Unlock()
		o.upstreamEventMu.Unlock()
		return true
	}
	o.mu.Lock()
	if o.upstream == proc {
		o.upstream = nil
	}
	o.upstreamDead.Store(true)
	o.mu.Unlock()
	o.retiringProcess = nil
	o.materializationBlockedErr = nil
	o.materializationState = MaterializationCacheOnly
	if o.cacheStage != nil && o.cacheStage.process == proc {
		o.cacheStage = nil
	}
	responses := o.claimInflightRequests()
	o.materializationMu.Unlock()
	o.upstreamEventMu.Unlock()
	o.deliverInflightResponses(responses)
	o.clearProactiveTracking()
	if o.shouldRetryMaterialization(MaterializationTriggerUpstreamExit) {
		o.startMaterialization(MaterializationTriggerUpstreamExit)
	} else if !o.hasUsableCache() {
		o.notifyUpstreamExit()
	}
	return true
}

// RestartPin freezes new materialization generations while restart state is
// serialized. Release must be called after the daemon has committed or aborted
// the snapshot operation.
type RestartPin struct {
	owner    *Owner
	Snapshot OwnerSnapshot
	Sessions []SessionSnapshot
	release  sync.Once
}

func (p *RestartPin) Release() {
	if p == nil || p.owner == nil {
		return
	}
	p.release.Do(p.owner.releaseRestartPin)
}

func (o *Owner) releaseRestartPin() {
	if o.restartPins.Add(-1) == 0 {
		o.resumeMaterializationAfterRestartPin()
	}
}

func (o *Owner) resumeMaterializationAfterRestartPin() {
	if o.restartPins.Load() != 0 {
		return
	}
	select {
	case <-o.materializationStop:
		o.materializationMu.Lock()
		o.restartResumeTrigger = ""
		o.materializationMu.Unlock()
		return
	default:
	}

	o.materializationMu.Lock()
	if o.restartPins.Load() != 0 {
		o.materializationMu.Unlock()
		return
	}
	if o.materializationAttempt != nil {
		o.materializationMu.Unlock()
		return
	}
	if o.materializationState == MaterializationReady || o.materializationState == MaterializationFinalizeBlocked {
		o.restartResumeTrigger = ""
		o.materializationMu.Unlock()
		return
	}
	trigger := o.restartResumeTrigger
	if o.materializationPolicy == MaterializationPersistent || o.persistentPending {
		trigger = MaterializationTriggerPersistent
	} else if trigger == "" && len(o.pendingDemands) > 0 {
		trigger = MaterializationTriggerDemand
	}
	o.restartResumeTrigger = ""
	if trigger == "" {
		o.materializationMu.Unlock()
		return
	}
	a, start := o.startMaterializationLocked(trigger)
	o.materializationMu.Unlock()
	if start {
		go o.runMaterialization(a)
	}
}

func (o *Owner) AcquireRestartPin(ctx context.Context) (*RestartPin, error) {
	o.restartPins.Add(1)
	releaseOnError := true
	defer func() {
		if releaseOnError {
			o.releaseRestartPin()
		}
	}()

	o.materializationMu.Lock()
	a := o.materializationAttempt
	var launch *LaunchContext
	if a != nil {
		if !a.launchFrozen {
			a.launch = o.electLaunchContextLocked()
			a.launchFrozen = true
		}
		selected := LaunchContext{SessionID: a.launch.SessionID, Cwd: a.launch.Cwd, Env: cloneLaunchEnv(a.launch.Env)}
		launch = &selected
		if o.restartResumeTrigger == "" {
			o.restartResumeTrigger = a.trigger
		}
	} else if o.materializationPolicy == MaterializationPersistent || o.persistentPending {
		o.restartResumeTrigger = MaterializationTriggerPersistent
	}
	o.materializationMu.Unlock()
	if a != nil {
		settled := false
		timer := time.NewTimer(restartMaterializationSettleGrace)
		select {
		case <-a.done:
			settled = true
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("restart materialization barrier: %w", ctx.Err())
		case <-o.materializationStop:
			timer.Stop()
			return nil, errors.New("restart materialization barrier: owner shutting down")
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		if !settled {
			a.cancelMaterialization()
			select {
			case <-a.done:
			case <-ctx.Done():
				return nil, fmt.Errorf("restart materialization cancellation: %w", ctx.Err())
			case <-o.materializationStop:
				return nil, errors.New("restart materialization cancellation: owner shutting down")
			}
		}
	}

	o.materializationMu.Lock()
	state := o.materializationState
	blockedErr := o.materializationBlockedErr
	o.materializationMu.Unlock()
	if state == MaterializationFinalizeBlocked {
		if blockedErr == nil {
			blockedErr = errFinalizationUnproven
		}
		return nil, fmt.Errorf("restart materialization barrier: %w", blockedErr)
	}
	snapshot, sessions := o.ExportSnapshotState()
	if launch != nil {
		snapshot.Cwd = launch.Cwd
		snapshot.Env = cloneSnapshotEnv(launch.Env)
	}
	pin := &RestartPin{owner: o, Snapshot: snapshot, Sessions: sessions}
	releaseOnError = false
	return pin, nil
}

func (o *Owner) quiesceMaterializationForHandoff() error {
	o.materializationMu.Lock()
	a := o.materializationAttempt
	o.materializationMu.Unlock()
	if a != nil {
		timer := time.NewTimer(materializationFinalizeTimeout)
		select {
		case <-a.done:
			timer.Stop()
		case <-timer.C:
			o.stopMaterialization()
			return fmt.Errorf("materialization quiesce timeout after %s", materializationFinalizeTimeout)
		}
	}
	o.stopMaterialization()
	o.materializationMu.Lock()
	state := o.materializationState
	blockedErr := o.materializationBlockedErr
	o.materializationMu.Unlock()
	if state == MaterializationFinalizeBlocked {
		if blockedErr == nil {
			blockedErr = errFinalizationUnproven
		}
		return blockedErr
	}
	return nil
}

func (o *Owner) hasUsableCache() bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.initResp != nil
}

func (o *Owner) CacheReady() bool {
	o.mu.RLock()
	ready := o.initResp != nil && o.toolList != nil
	o.mu.RUnlock()
	return ready
}

func (o *Owner) stopMaterialization() {
	if o.materializationStop != nil {
		select {
		case <-o.materializationStop:
			return
		default:
			close(o.materializationStop)
		}
	}
	o.materializationMu.Lock()
	if a := o.materializationAttempt; a != nil {
		a.signals.signalFailure(errors.New("owner shutting down"))
	}
	o.materializationMu.Unlock()
	o.failAllLocalDemands(errors.New("owner shutting down"))
}

func (o *Owner) MaterializationState() MaterializationState {
	o.materializationMu.Lock()
	state := o.materializationState
	o.materializationMu.Unlock()
	return state
}

func (o *Owner) PersistentPending() bool {
	o.materializationMu.Lock()
	pending := o.persistentPending
	o.materializationMu.Unlock()
	return pending
}

func (o *Owner) ResolvePersistent(persistent bool) {
	o.materializationMu.Lock()
	o.persistentPending = false
	if persistent || o.persistentRequired {
		o.materializationPolicy = MaterializationPersistent
	} else if o.materializationPolicy == MaterializationPersistent {
		o.materializationPolicy = MaterializationOnDemand
	}
	o.materializationMu.Unlock()
}

func (o *Owner) MaterializationBlocksEviction() bool {
	if o.restartPins.Load() > 0 {
		return true
	}
	o.materializationMu.Lock()
	state := o.materializationState
	pendingPersistent := o.persistentPending
	o.materializationMu.Unlock()
	return pendingPersistent || state == MaterializationMaterializing || state == MaterializationFinalizing || state == MaterializationFinalizeBlocked
}

func (o *Owner) materializationStatus() (MaterializationState, MaterializationTrigger, MaterializationPolicy, uint64, int, bool, string) {
	o.materializationMu.Lock()
	defer o.materializationMu.Unlock()
	blocked := ""
	if o.materializationBlockedErr != nil {
		blocked = o.materializationBlockedErr.Error()
	}
	return o.materializationState, o.materializationTrigger, o.materializationPolicy,
		o.materializationGeneration, len(o.pendingDemands), o.persistentPending, blocked
}
