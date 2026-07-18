package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

func (runner *runner) replaceCurrent(reason Reason) {
	if runner.terminal {
		return
	}
	if runner.current == nil {
		runner.beginStart()
		return
	}

	runner.stopTimer(&runner.replayTimer)
	runner.stopTimer(&runner.dormantExitTimer)
	runner.stopTimer(&runner.childExitDrainTimer)
	runner.stopTimer(&runner.dormantLeaseTimer)
	runner.resolving = false
	runner.resolveID++
	runner.setState(StateFinalizing, reason)
	runner.commitQuiescingCancellations()
	runner.pending.discardSameGenerationOnly()

	generation := runner.current.id
	lost := runner.host.retireGeneration(generation)
	runner.child.retireGeneration(generation)
	for _, record := range lost {
		if err := runner.writeHost(localErrorFrame(record.id.raw, codeInternal, lostRequestMessage)); err != nil {
			runner.terminate(ReasonHostOutputFailure, err)
			return
		}
	}
	if err := runner.finalizeCurrent(); err != nil {
		runner.terminate(ReasonProtocolFailure, err)
		return
	}
	if !runner.terminal {
		runner.beginStart()
	}
}

func (runner *runner) finalizeCurrent() error {
	generation := runner.current
	if generation == nil {
		return nil
	}
	generation.cancel()

	exit, exited := generation.exitResult()
	if !exited {
		stopCtx, cancel := context.WithTimeout(context.Background(), runner.cfg.GracefulStopTimeout)
		stopErr := generation.child.Stop(stopCtx)
		exit, exited = generation.exitResult()
		if !exited {
			select {
			case <-generation.exitDone:
				exit, exited = generation.exitResult()
			case <-stopCtx.Done():
			}
		}
		if stopErr != nil {
			cancel()
			return fmt.Errorf("supervisor: child finalization lacks terminal proof of process-tree retirement: %w", stopErr)
		}
		if !exited {
			proofErr := context.Cause(stopCtx)
			cancel()
			return fmt.Errorf("supervisor: child finalization lacks terminal proof: %w", proofErr)
		}
		cancel()
	}
	if errors.Is(exit.Err, ErrProcessTreeUnproven) {
		return fmt.Errorf("supervisor: child finalization lacks terminal proof of process-tree retirement: %w", exit.Err)
	}

	if err := runner.joinPump(generation); err != nil {
		return err
	}
	if err := runner.joinReaper(generation); err != nil {
		return err
	}
	_ = generation.stdin.Close()
	runner.current = nil
	if generation.admission != nil {
		if err := generation.admission.Close(); err != nil {
			return fmt.Errorf("supervisor: admission cleanup failed: %w", err)
		}
	}
	return nil
}

func (runner *runner) joinPump(generation *generation) error {
	timer := runner.cfg.runtimeClock.NewTimer(runner.cfg.OutputDrainTimeout)
	select {
	case <-generation.pumpDone:
		timer.Stop()
		if generation.stdout != nil {
			_ = generation.stdout.Close()
		}
		return nil
	case <-timer.C():
	}
	if generation.stdout != nil {
		_ = generation.stdout.Close()
	}

	timer = runner.cfg.runtimeClock.NewTimer(runner.cfg.GracefulStopTimeout)
	defer timer.Stop()
	select {
	case <-generation.pumpDone:
		return nil
	case <-timer.C():
		return fmt.Errorf("supervisor: child stdout pump did not terminate")
	}
}

func (runner *runner) joinReaper(generation *generation) error {
	timer := runner.cfg.runtimeClock.NewTimer(runner.cfg.GracefulStopTimeout)
	defer timer.Stop()
	select {
	case <-generation.reapDone:
		return nil
	case <-timer.C():
		return fmt.Errorf("supervisor: child reaper sender did not terminate")
	}
}

func (runner *runner) handleChildOutput(err error) {
	runner.stopTimer(&runner.childExitDrainTimer)
	if runner.state == StateAwaitingDormantExit && errors.Is(err, io.EOF) {
		runner.maybeCompleteDormancy()
		return
	}
	if runner.state == StateAwaitingDormantExit {
		runner.dormancyInvalid = true
		runner.replaceCurrent(ReasonDormantRejected)
		return
	}
	if runner.state == StateFinalizing || runner.state == StateStopping {
		return
	}
	runner.replaceCurrent(ReasonOutputEOF)
}

func (runner *runner) handleChildExit(exit Exit) {
	if runner.state == StateAwaitingDormantExit {
		runner.maybeCompleteDormancy()
		return
	}
	if runner.state == StateFinalizing || runner.state == StateStopping {
		return
	}
	_ = exit
	outputEOF, outputErr := runner.current.outputTerminal()
	if outputEOF || outputErr != nil {
		runner.replaceCurrent(ReasonCrash)
		return
	}
	if runner.childExitDrainTimer == nil {
		runner.childExitDrainTimer = runner.cfg.runtimeClock.NewTimer(runner.cfg.OutputDrainTimeout)
	}
}

func (runner *runner) maybeCompleteDormancy() {
	generation := runner.current
	if generation == nil || runner.state != StateAwaitingDormantExit {
		return
	}
	exit, exited := generation.exitResult()
	outputEOF, outputErr := generation.outputTerminal()
	if !exited || !outputEOF {
		if outputErr != nil && !errors.Is(outputErr, io.EOF) {
			runner.dormancyInvalid = true
			runner.replaceCurrent(ReasonDormantRejected)
		}
		return
	}
	if exit.Code != ProtocolV2().DormantExitCode() || exit.Err != nil || runner.dormancyInvalid {
		runner.replaceCurrent(ReasonDormantRejected)
		return
	}

	runner.stopTimer(&runner.dormantExitTimer)
	runner.setState(StateFinalizing, ReasonDormantCommit)
	generationID := generation.id
	lost := runner.host.retireGeneration(generationID)
	runner.child.retireGeneration(generationID)
	for _, record := range lost {
		if err := runner.writeHost(localErrorFrame(record.id.raw, codeInternal, lostRequestMessage)); err != nil {
			runner.terminate(ReasonHostOutputFailure, err)
			return
		}
	}
	if err := runner.finalizeCurrent(); err != nil {
		runner.terminate(ReasonProtocolFailure, err)
		return
	}
	sequence, _ := runner.hostBoundary.snapshot()
	if runner.pending.len() > 0 || sequence > runner.dormantTransitionStart {
		runner.beginStart()
		return
	}
	runner.setState(StateDormant, ReasonDormantCommit)
	runner.armDormantLease()
}

func (runner *runner) armDormantLease() {
	if runner.cfg.DormantLease <= 0 {
		return
	}
	runner.dormantLeaseDeadline = runner.cfg.runtimeClock.Now().Add(runner.cfg.DormantLease)
	runner.dormantLeaseTimer = runner.cfg.runtimeClock.NewTimer(runner.cfg.DormantLease)
}

func (runner *runner) handleDormantLease(now time.Time) {
	if runner.state != StateDormant || runner.dormantLeaseTimer == nil {
		return
	}
	runner.stopTimer(&runner.dormantLeaseTimer)
	sequence, arrived := runner.hostBoundary.snapshot()
	if sequence > runner.lastHostSequence && !arrived.After(runner.dormantLeaseDeadline) {
		return
	}
	runner.finishDormantLease(now)
}

func (runner *runner) resumeDormantLeaseAfterHost() {
	if runner.terminal || runner.state != StateDormant || runner.dormantLeaseTimer != nil || runner.cfg.DormantLease <= 0 {
		return
	}
	sequence, arrived := runner.hostBoundary.snapshot()
	if sequence > runner.lastHostSequence && !arrived.After(runner.dormantLeaseDeadline) {
		return
	}
	runner.finishDormantLease(runner.cfg.runtimeClock.Now())
}

func (runner *runner) finishDormantLease(now time.Time) {
	if now.Before(runner.dormantLeaseDeadline) {
		duration := runner.dormantLeaseDeadline.Sub(now)
		runner.dormantLeaseTimer = runner.cfg.runtimeClock.NewTimer(duration)
		return
	}
	runner.terminate(ReasonDormantCommit, nil)
}
