package supervisor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
)

const (
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
	codePendingFull    = -32000
)

const lostRequestMessage = "mcp-mux engine restarted, request lost during launcher reconnect"

type requestRecord struct {
	id            scalarValue
	method        string
	initialize    bool
	generation    uint64
	progressToken *scalarValue
	taskAugmented bool
	taskID        string
}

func (record *requestRecord) taskRelated() bool {
	return record != nil && (record.taskAugmented || record.taskID != "")
}

type tokenRecord struct {
	requestID  scalarKey
	generation uint64
	last       string
	hasLast    bool
}

type taskRecord struct {
	id            string
	requestID     scalarKey
	generation    uint64
	progressToken *scalarKey
	status        string
}

type currentTaskState uint8

const (
	currentTaskActive currentTaskState = iota + 1
	currentTaskTerminal
	currentTaskFinalized
)

const (
	tombstoneLimit           = 1024
	persistentFenceLimit     = 1024
	activeCorrelationLimit   = 256
	retainedCorrelationLimit = tombstoneLimit + activeCorrelationLimit
	terminalTaskLimit        = retainedCorrelationLimit - activeCorrelationLimit
	maxCorrelationValueBytes = 16 * 1024
)

type tombstoneRole uint8

const (
	tombstoneRequest tombstoneRole = iota + 1
	tombstoneToken
	tombstoneTask
)

type tombstoneKey struct {
	role tombstoneRole
	key  scalarKey
}

func taskTombstoneKey(id string) scalarKey {
	return scalarKey{kind: scalarString, value: id}
}

type tombstoneSet struct {
	values map[tombstoneKey]struct{}
	order  []tombstoneKey
	next   int
}

func (set *tombstoneSet) contains(role tombstoneRole, key scalarKey) bool {
	_, ok := set.values[tombstoneKey{role: role, key: key}]
	return ok
}

func (set *tombstoneSet) add(role tombstoneRole, key scalarKey) {
	entry := tombstoneKey{role: role, key: key}
	if set.values == nil {
		set.values = make(map[tombstoneKey]struct{})
	}
	if _, exists := set.values[entry]; exists {
		return
	}
	set.values[entry] = struct{}{}
	if len(set.order) < tombstoneLimit {
		set.order = append(set.order, entry)
		return
	}
	delete(set.values, set.order[set.next])
	set.order[set.next] = entry
	set.next = (set.next + 1) % tombstoneLimit
}

func (set *tombstoneSet) clear() {
	clear(set.values)
	set.order = set.order[:0]
	set.next = 0
}

type persistentFence struct {
	values [tombstoneTask + 1]map[scalarKey]struct{}
}

func (fence *persistentFence) contains(role tombstoneRole, key scalarKey) bool {
	if role < tombstoneRequest || role > tombstoneTask {
		return false
	}
	_, ok := fence.values[role][key]
	return ok
}

func (fence *persistentFence) add(role tombstoneRole, key scalarKey) error {
	if role < tombstoneRequest || role > tombstoneTask {
		return fmt.Errorf("invalid persistent fence role %d", role)
	}
	values := fence.values[role]
	if _, exists := values[key]; exists {
		return nil
	}
	if len(values) >= persistentFenceLimit {
		return fmt.Errorf("persistent fence limit reached for role %d", role)
	}
	if values == nil {
		values = make(map[scalarKey]struct{})
		fence.values[role] = values
	}
	values[key] = struct{}{}
	return nil
}

func (fence *persistentFence) addMany(entries ...tombstoneKey) error {
	pending := make(map[tombstoneKey]struct{}, len(entries))
	var additions [tombstoneTask + 1]int
	for _, entry := range entries {
		if entry.role < tombstoneRequest || entry.role > tombstoneTask {
			return fmt.Errorf("invalid persistent fence role %d", entry.role)
		}
		if fence.contains(entry.role, entry.key) {
			continue
		}
		if _, exists := pending[entry]; exists {
			continue
		}
		pending[entry] = struct{}{}
		additions[entry.role]++
	}
	for role := tombstoneRequest; role <= tombstoneTask; role++ {
		if len(fence.values[role])+additions[role] > persistentFenceLimit {
			return fmt.Errorf("persistent fence limit reached for role %d", role)
		}
	}
	for entry := range pending {
		if err := fence.add(entry.role, entry.key); err != nil {
			return err
		}
	}
	return nil
}

func (fence *persistentFence) clear() {
	for role := tombstoneRequest; role <= tombstoneTask; role++ {
		clear(fence.values[role])
	}
}

type directionRegistry struct {
	requests         map[scalarKey]*requestRecord
	tokens           map[scalarKey]*tokenRecord
	tasks            map[string]*taskRecord
	terminalTasks    map[string]*taskRecord
	tombstones       tombstoneSet
	persistentFences persistentFence
	retiredRequests  map[scalarKey]*requestRecord
	retiredTokens    map[scalarKey]struct{}
	retiredTasks     map[string]*taskRecord
}

func newDirectionRegistry() directionRegistry {
	return directionRegistry{
		requests:        make(map[scalarKey]*requestRecord),
		tokens:          make(map[scalarKey]*tokenRecord),
		tasks:           make(map[string]*taskRecord),
		terminalTasks:   make(map[string]*taskRecord),
		retiredRequests: make(map[scalarKey]*requestRecord),
		retiredTokens:   make(map[scalarKey]struct{}),
		retiredTasks:    make(map[string]*taskRecord),
	}
}

func (registry *directionRegistry) hasTaskLifecycle() bool {
	if len(registry.tasks) > 0 || len(registry.terminalTasks) > 0 {
		return true
	}
	for _, record := range registry.requests {
		if record.taskAugmented {
			return true
		}
	}
	return false
}

func (registry *directionRegistry) canRegister(frame *parsedFrame) error {
	if frame.id == nil {
		return fmt.Errorf("request id is missing")
	}
	if _, exists := registry.retiredRequests[frame.id.key]; exists {
		return fmt.Errorf("request id belongs to a retired generation")
	}
	if registry.tombstones.contains(tombstoneRequest, frame.id.key) || registry.persistentFences.contains(tombstoneRequest, frame.id.key) {
		return fmt.Errorf("request id is tombstoned")
	}
	if _, exists := registry.requests[frame.id.key]; exists {
		return fmt.Errorf("duplicate active request id")
	}
	activeLimit := activeCorrelationLimit
	if frame.taskOperation == "" && (frame.taskAugmented || registry.hasTaskLifecycle()) {
		activeLimit--
	}
	if len(registry.requests)+len(registry.tasks) >= activeLimit {
		return fmt.Errorf("active correlation limit reached")
	}
	if len(registry.requests)+len(registry.retiredRequests) >= retainedCorrelationLimit {
		return fmt.Errorf("request correlation retention limit reached")
	}
	if frame.taskAugmented && len(registry.tasks)+len(registry.terminalTasks)+len(registry.retiredTasks) >= retainedCorrelationLimit {
		return fmt.Errorf("task correlation retention limit reached")
	}
	if frame.progressToken != nil {
		if _, exists := registry.retiredTokens[frame.progressToken.key]; exists {
			return fmt.Errorf("progress token belongs to a retired generation")
		}
		if registry.tombstones.contains(tombstoneToken, frame.progressToken.key) || registry.persistentFences.contains(tombstoneToken, frame.progressToken.key) {
			return fmt.Errorf("progress token is tombstoned")
		}
		if _, exists := registry.tokens[frame.progressToken.key]; exists {
			return fmt.Errorf("duplicate active progress token")
		}
		if len(registry.tokens)+len(registry.retiredTokens) >= retainedCorrelationLimit {
			return fmt.Errorf("progress token correlation retention limit reached")
		}
	}
	return nil
}

func (registry *directionRegistry) register(frame *parsedFrame, generation uint64) *requestRecord {
	record := &requestRecord{
		id:            *frame.id,
		method:        frame.method,
		initialize:    frame.method == "initialize",
		generation:    generation,
		progressToken: frame.progressToken,
		taskAugmented: frame.taskAugmented,
		taskID:        frame.taskID,
	}
	registry.requests[record.id.key] = record
	if record.progressToken != nil {
		registry.tokens[record.progressToken.key] = &tokenRecord{
			requestID:  record.id.key,
			generation: generation,
		}
	}
	return record
}

func (registry *directionRegistry) canBindTask(taskID string) error {
	if _, exists := registry.tasks[taskID]; exists {
		return fmt.Errorf("taskId belongs to an active task")
	}
	if _, exists := registry.terminalTasks[taskID]; exists {
		return fmt.Errorf("taskId belongs to a terminal task")
	}
	if _, exists := registry.retiredTasks[taskID]; exists {
		return fmt.Errorf("taskId belongs to a retired generation")
	}
	taskKey := taskTombstoneKey(taskID)
	if registry.tombstones.contains(tombstoneTask, taskKey) || registry.persistentFences.contains(tombstoneTask, taskKey) {
		return fmt.Errorf("taskId is tombstoned")
	}
	if len(registry.tasks)+len(registry.terminalTasks)+len(registry.retiredTasks) >= retainedCorrelationLimit {
		return fmt.Errorf("task correlation retention limit reached")
	}
	return nil
}

func (registry *directionRegistry) currentTaskForOperation(record *requestRecord) (*taskRecord, currentTaskState, error) {
	taskKey := taskTombstoneKey(record.taskID)
	active := registry.tasks[record.taskID]
	terminal := registry.terminalTasks[record.taskID]
	_, retired := registry.retiredTasks[record.taskID]
	fenced := registry.persistentFences.contains(tombstoneTask, taskKey)
	transitioned := registry.tombstones.contains(tombstoneTask, taskKey)
	if active != nil {
		if active.generation != record.generation || terminal != nil || retired || fenced || transitioned {
			return nil, 0, fmt.Errorf("task operation has ambiguous active taskId state")
		}
		return active, currentTaskActive, nil
	}
	if terminal != nil {
		if terminal.generation != record.generation || retired || fenced || transitioned {
			return nil, 0, fmt.Errorf("task operation has ambiguous terminal taskId state")
		}
		return terminal, currentTaskTerminal, nil
	}
	if retired || transitioned {
		return nil, 0, fmt.Errorf("task operation collides with retired taskId state")
	}
	if fenced {
		return nil, currentTaskFinalized, nil
	}
	return nil, 0, fmt.Errorf("unknown taskId")
}

func (registry *directionRegistry) canComplete(record *requestRecord, response *parsedFrame) error {
	if meta, handled, err := taskOperationResponse(record, response); handled {
		if err != nil {
			return err
		}
		task, state, stateErr := registry.currentTaskForOperation(record)
		if stateErr != nil {
			return stateErr
		}
		if meta == nil {
			return nil
		}
		if (meta.status == "working" || meta.status == "input_required") && state != currentTaskActive {
			return fmt.Errorf("nonterminal task operation response follows terminal task status")
		}
		if record.method == "tasks/result" || meta.status == "working" || meta.status == "input_required" {
			return nil
		}
		switch state {
		case currentTaskActive:
			if len(registry.terminalTasks) >= terminalTaskLimit {
				return fmt.Errorf("terminal task retention limit reached")
			}
		case currentTaskTerminal:
			if task.status != meta.status {
				return fmt.Errorf("terminal task status changed from %s to %s", task.status, meta.status)
			}
		}
		return nil
	}
	if !record.taskAugmented || response.kind == frameError {
		return nil
	}
	if response.utilityErr != nil {
		return fmt.Errorf("invalid task result: %w", response.utilityErr)
	}
	if response.kind != frameResult || response.taskResult == nil {
		return fmt.Errorf("task-augmented request requires CreateTaskResult")
	}
	meta := response.taskResult
	if meta.status != "working" {
		return fmt.Errorf("CreateTaskResult status must be working")
	}
	return registry.canBindTask(meta.id)
}

func (registry *directionRegistry) fenceRequest(record *requestRecord, includeToken bool) error {
	if record.taskRelated() {
		entries := []tombstoneKey{{role: tombstoneRequest, key: record.id.key}}
		if includeToken && record.progressToken != nil {
			entries = append(entries, tombstoneKey{role: tombstoneToken, key: record.progressToken.key})
		}
		return registry.persistentFences.addMany(entries...)
	}
	registry.tombstones.add(tombstoneRequest, record.id.key)
	if includeToken && record.progressToken != nil {
		registry.tombstones.add(tombstoneToken, record.progressToken.key)
	}
	return nil
}

func (registry *directionRegistry) complete(record *requestRecord, response *parsedFrame) error {
	operationMeta, operation, err := taskOperationResponse(record, response)
	if err != nil {
		return err
	}
	bindTask := !operation && record.taskAugmented && response.kind == frameResult && response.taskResult != nil && response.taskResult.status == "working"
	if err := registry.fenceRequest(record, !bindTask); err != nil {
		return err
	}
	if operation && operationMeta != nil {
		var handled bool
		if record.method == "tasks/result" {
			handled, err = registry.finalizeTask(record.taskID, record.generation)
		} else {
			handled, err = registry.updateTask(operationMeta, record.generation)
		}
		if err != nil {
			return err
		}
		if !handled {
			return fmt.Errorf("unknown taskId")
		}
	}

	delete(registry.requests, record.id.key)
	if operation {
		registry.removeToken(record)
		return nil
	}

	if bindTask {
		meta := response.taskResult
		task := &taskRecord{
			id:         meta.id,
			requestID:  record.id.key,
			generation: record.generation,
			status:     meta.status,
		}
		if record.progressToken != nil {
			tokenKey := record.progressToken.key
			task.progressToken = &tokenKey
		}
		registry.tasks[meta.id] = task
		return nil
	}
	registry.removeToken(record)
	return nil
}

func (registry *directionRegistry) cancel(record *requestRecord) error {
	if err := registry.fenceRequest(record, true); err != nil {
		return err
	}
	delete(registry.requests, record.id.key)
	registry.removeToken(record)
	return nil
}

func (registry *directionRegistry) retireRequest(record *requestRecord) {
	delete(registry.requests, record.id.key)
	registry.retiredRequests[record.id.key] = record
	if record.progressToken != nil {
		delete(registry.tokens, record.progressToken.key)
		registry.retiredTokens[record.progressToken.key] = struct{}{}
	}
}

func (registry *directionRegistry) removeToken(record *requestRecord) {
	if record.progressToken != nil {
		delete(registry.tokens, record.progressToken.key)
	}
}

func (registry *directionRegistry) updateTask(meta *taskMeta, generation uint64) (bool, error) {
	active := registry.tasks[meta.id]
	terminal := registry.terminalTasks[meta.id]
	if active != nil && terminal != nil {
		return true, fmt.Errorf("taskId belongs to active and terminal tasks")
	}
	if active != nil {
		if active.generation != generation {
			return false, nil
		}
		if meta.status == "working" || meta.status == "input_required" {
			active.status = meta.status
			return true, nil
		}
		if len(registry.terminalTasks) >= terminalTaskLimit {
			return true, fmt.Errorf("terminal task retention limit reached")
		}
		if active.progressToken != nil {
			if err := registry.persistentFences.add(tombstoneToken, *active.progressToken); err != nil {
				return true, err
			}
		}
		delete(registry.tasks, meta.id)
		if active.progressToken != nil {
			delete(registry.tokens, *active.progressToken)
		}
		active.status = meta.status
		registry.terminalTasks[meta.id] = active
		return true, nil
	}
	if terminal != nil {
		if terminal.generation != generation {
			return false, nil
		}
		if meta.status == "working" || meta.status == "input_required" {
			return true, fmt.Errorf("nonterminal task status follows terminal task status")
		}
		if terminal.status != meta.status {
			return true, fmt.Errorf("terminal task status changed from %s to %s", terminal.status, meta.status)
		}
		return true, nil
	}
	if registry.persistentFences.contains(tombstoneTask, taskTombstoneKey(meta.id)) {
		if meta.status == "working" || meta.status == "input_required" {
			return true, fmt.Errorf("nonterminal task status follows finalized task")
		}
		return true, nil
	}
	return false, nil
}

func (registry *directionRegistry) finalizeTask(taskID string, generation uint64) (bool, error) {
	active := registry.tasks[taskID]
	terminal := registry.terminalTasks[taskID]
	if active != nil && terminal != nil {
		return true, fmt.Errorf("taskId belongs to active and terminal tasks")
	}
	if _, retired := registry.retiredTasks[taskID]; retired {
		return true, fmt.Errorf("taskId belongs to current and retired task state")
	}
	task := active
	if task == nil {
		task = terminal
	}
	if task == nil {
		if registry.persistentFences.contains(tombstoneTask, taskTombstoneKey(taskID)) {
			return true, nil
		}
		return false, nil
	}
	if task.generation != generation {
		return false, nil
	}
	entries := []tombstoneKey{{role: tombstoneTask, key: taskTombstoneKey(taskID)}}
	if task.progressToken != nil {
		entries = append(entries, tombstoneKey{role: tombstoneToken, key: *task.progressToken})
	}
	if err := registry.persistentFences.addMany(entries...); err != nil {
		return true, err
	}
	delete(registry.tasks, taskID)
	delete(registry.terminalTasks, taskID)
	if task.progressToken != nil {
		delete(registry.tokens, *task.progressToken)
	}
	return true, nil
}

func (registry *directionRegistry) acceptProgress(meta *progressMeta, generation uint64) bool {
	token := registry.tokens[meta.token.key]
	if token == nil || token.generation != generation {
		return false
	}
	if token.hasLast && compareCanonicalNumbers(meta.progress, token.last) <= 0 {
		return false
	}
	token.last = meta.progress
	token.hasLast = true
	return true
}

func (registry *directionRegistry) retireGenerationChecked(generation uint64) ([]*requestRecord, error) {
	if len(registry.requests)+len(registry.retiredRequests) > retainedCorrelationLimit {
		return nil, fmt.Errorf("request correlation retention limit exceeded")
	}
	if len(registry.tokens)+len(registry.retiredTokens) > retainedCorrelationLimit {
		return nil, fmt.Errorf("progress token correlation retention limit exceeded")
	}
	if len(registry.tasks)+len(registry.terminalTasks)+len(registry.retiredTasks) > retainedCorrelationLimit {
		return nil, fmt.Errorf("task correlation retention limit exceeded")
	}
	if len(registry.terminalTasks) > terminalTaskLimit {
		return nil, fmt.Errorf("terminal task retention limit exceeded")
	}

	entries := make([]tombstoneKey, 0)
	for _, record := range registry.requests {
		if record.generation != generation {
			continue
		}
		if _, exists := registry.retiredRequests[record.id.key]; exists {
			return nil, fmt.Errorf("request id belongs to active and retired generations")
		}
	}
	for id, task := range registry.tasks {
		if task.generation != generation {
			continue
		}
		if _, exists := registry.retiredTasks[id]; exists {
			return nil, fmt.Errorf("taskId belongs to active and retired generations")
		}
		if _, exists := registry.terminalTasks[id]; exists {
			return nil, fmt.Errorf("taskId belongs to active and terminal tasks")
		}
		if task.requestID.kind != 0 {
			entries = append(entries, tombstoneKey{role: tombstoneRequest, key: task.requestID})
		}
	}
	for id, task := range registry.terminalTasks {
		if task.generation != generation {
			continue
		}
		if _, exists := registry.tasks[id]; exists {
			return nil, fmt.Errorf("taskId belongs to active and terminal tasks")
		}
		if _, exists := registry.retiredTasks[id]; exists {
			return nil, fmt.Errorf("taskId belongs to terminal and retired tasks")
		}
		if registry.tombstones.contains(tombstoneTask, taskTombstoneKey(id)) || registry.persistentFences.contains(tombstoneTask, taskTombstoneKey(id)) {
			return nil, fmt.Errorf("terminal taskId is already fenced")
		}
		entries = append(entries, tombstoneKey{role: tombstoneTask, key: taskTombstoneKey(id)})
		if task.progressToken != nil {
			entries = append(entries, tombstoneKey{role: tombstoneToken, key: *task.progressToken})
		}
	}
	if err := registry.persistentFences.addMany(entries...); err != nil {
		return nil, err
	}

	lost := make([]*requestRecord, 0)
	for _, record := range registry.requests {
		if record.generation != generation {
			continue
		}
		registry.retireRequest(record)
		if !record.initialize {
			lost = append(lost, record)
		}
	}
	for id, task := range registry.tasks {
		if task.generation != generation {
			continue
		}
		delete(registry.tasks, id)
		registry.retiredTasks[id] = task
		if task.progressToken != nil {
			delete(registry.tokens, *task.progressToken)
			registry.retiredTokens[*task.progressToken] = struct{}{}
		}
	}
	for id, task := range registry.terminalTasks {
		if task.generation == generation {
			delete(registry.terminalTasks, id)
		}
	}
	sort.Slice(lost, func(i, j int) bool {
		if lost[i].id.key.kind != lost[j].id.key.kind {
			return lost[i].id.key.kind < lost[j].id.key.kind
		}
		return lost[i].id.key.value < lost[j].id.key.value
	})
	return lost, nil
}

// retireGeneration preserves the shutdown call shape. Run.shutdown is already
// terminal, so it may ignore a fence-cap error; every path that can continue
// routing or start a replacement must use retireGenerationChecked.
func (registry *directionRegistry) retireGeneration(generation uint64) []*requestRecord {
	lost, _ := registry.retireGenerationChecked(generation)
	return lost
}

func (registry *directionRegistry) consumeRetiredResponse(response *parsedFrame) (bool, error) {
	if response == nil || response.id == nil {
		return false, nil
	}
	record := registry.retiredRequests[response.id.key]
	if record == nil {
		return false, nil
	}

	operationMeta, operation, err := taskOperationResponse(record, response)
	if operation {
		if err != nil {
			return true, err
		}
		task, terminalFenced, taskErr := registry.retiredTaskForOperation(record)
		if taskErr != nil {
			return true, taskErr
		}
		if operationMeta == nil {
			if err := registry.releaseRetiredRequest(record); err != nil {
				return true, err
			}
			return true, nil
		}
		if operationMeta.status == "working" || operationMeta.status == "input_required" {
			if terminalFenced {
				return true, fmt.Errorf("nonterminal task operation response follows terminal task status")
			}
			if err := registry.releaseRetiredRequest(record); err != nil {
				return true, err
			}
			task.status = operationMeta.status
			return true, nil
		}
		if terminalFenced {
			if err := registry.releaseRetiredRequest(record); err != nil {
				return true, err
			}
			return true, nil
		}
		if err := registry.releaseRetiredTaskOperation(record, task); err != nil {
			return true, err
		}
		return true, nil
	}

	if !record.taskAugmented || response.kind == frameError {
		if err := registry.releaseRetiredRequest(record); err != nil {
			return true, err
		}
		return true, nil
	}
	if response.utilityErr != nil {
		return true, fmt.Errorf("retired CreateTaskResult: %w", response.utilityErr)
	}
	if response.kind != frameResult || response.taskResult == nil {
		return true, fmt.Errorf("retired task-augmented request requires CreateTaskResult")
	}
	meta := response.taskResult
	if meta.status != "working" {
		return true, fmt.Errorf("retired CreateTaskResult status must be working")
	}
	if err := registry.canBindTask(meta.id); err != nil {
		return true, fmt.Errorf("retired CreateTaskResult: %w", err)
	}
	if err := registry.persistentFences.add(tombstoneRequest, record.id.key); err != nil {
		return true, err
	}
	task := &taskRecord{
		id:         meta.id,
		requestID:  record.id.key,
		generation: record.generation,
		status:     meta.status,
	}
	if record.progressToken != nil {
		tokenKey := record.progressToken.key
		task.progressToken = &tokenKey
	}
	delete(registry.retiredRequests, record.id.key)
	registry.retiredTasks[meta.id] = task
	return true, nil
}

func (registry *directionRegistry) releaseRetiredRequest(record *requestRecord) error {
	if record.taskRelated() {
		entries := []tombstoneKey{{role: tombstoneRequest, key: record.id.key}}
		if record.progressToken != nil {
			entries = append(entries, tombstoneKey{role: tombstoneToken, key: record.progressToken.key})
		}
		if err := registry.persistentFences.addMany(entries...); err != nil {
			return err
		}
	} else {
		registry.tombstones.add(tombstoneRequest, record.id.key)
		if record.progressToken != nil {
			registry.tombstones.add(tombstoneToken, record.progressToken.key)
		}
	}
	delete(registry.retiredRequests, record.id.key)
	if record.progressToken != nil {
		delete(registry.retiredTokens, record.progressToken.key)
	}
	return nil
}

func (registry *directionRegistry) retiredTaskForOperation(record *requestRecord) (*taskRecord, bool, error) {
	taskKey := taskTombstoneKey(record.taskID)
	retired := registry.retiredTasks[record.taskID]
	_, active := registry.tasks[record.taskID]
	_, terminal := registry.terminalTasks[record.taskID]
	fenced := registry.persistentFences.contains(tombstoneTask, taskKey)
	transitioned := registry.tombstones.contains(tombstoneTask, taskKey)
	if retired == nil {
		if active || terminal || transitioned {
			return nil, false, fmt.Errorf("retired task operation collides with current taskId state")
		}
		if fenced {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("retired task operation references unknown taskId")
	}
	if active || terminal || fenced || transitioned {
		return nil, false, fmt.Errorf("retired task operation has ambiguous taskId state")
	}
	return retired, false, nil
}

func (registry *directionRegistry) releaseRetiredTaskOperation(record *requestRecord, task *taskRecord) error {
	entries := []tombstoneKey{
		{role: tombstoneRequest, key: record.id.key},
		{role: tombstoneTask, key: taskTombstoneKey(task.id)},
	}
	if record.progressToken != nil {
		entries = append(entries, tombstoneKey{role: tombstoneToken, key: record.progressToken.key})
	}
	if task.progressToken != nil {
		entries = append(entries, tombstoneKey{role: tombstoneToken, key: *task.progressToken})
	}
	if err := registry.persistentFences.addMany(entries...); err != nil {
		return err
	}
	delete(registry.retiredRequests, record.id.key)
	if record.progressToken != nil {
		delete(registry.retiredTokens, record.progressToken.key)
	}
	delete(registry.retiredTasks, task.id)
	if task.progressToken != nil {
		delete(registry.retiredTokens, *task.progressToken)
	}
	return nil
}

func (registry *directionRegistry) consumeRetiredTaskStatus(meta *taskMeta) (bool, error) {
	if meta == nil {
		return false, nil
	}
	taskKey := taskTombstoneKey(meta.id)
	task := registry.retiredTasks[meta.id]
	if task == nil {
		if !registry.persistentFences.contains(tombstoneTask, taskKey) {
			return false, nil
		}
		if _, exists := registry.tasks[meta.id]; exists {
			return true, fmt.Errorf("taskId belongs to an active task and a persistent fence")
		}
		if _, exists := registry.terminalTasks[meta.id]; exists {
			return true, fmt.Errorf("taskId belongs to a terminal task and a persistent fence")
		}
		return true, nil
	}
	if _, exists := registry.tasks[meta.id]; exists {
		return true, fmt.Errorf("taskId belongs to active and retired generations")
	}
	if _, exists := registry.terminalTasks[meta.id]; exists {
		return true, fmt.Errorf("taskId belongs to terminal and retired generations")
	}
	if registry.tombstones.contains(tombstoneTask, taskKey) || registry.persistentFences.contains(tombstoneTask, taskKey) {
		return true, fmt.Errorf("retired taskId is also fenced")
	}
	if meta.status == "working" || meta.status == "input_required" {
		task.status = meta.status
		return true, nil
	}
	entries := []tombstoneKey{{role: tombstoneTask, key: taskKey}}
	if task.progressToken != nil {
		entries = append(entries, tombstoneKey{role: tombstoneToken, key: *task.progressToken})
	}
	if err := registry.persistentFences.addMany(entries...); err != nil {
		return true, err
	}
	delete(registry.retiredTasks, meta.id)
	if task.progressToken != nil {
		delete(registry.retiredTokens, *task.progressToken)
	}
	return true, nil
}

func (registry *directionRegistry) clearTransitionTombstones() {
	registry.tombstones.clear()
}

func (registry *directionRegistry) clearRetiredCorrelations() {
	clear(registry.retiredRequests)
	clear(registry.retiredTokens)
	clear(registry.retiredTasks)
	registry.persistentFences.clear()
}

func localErrorFrame(id json.RawMessage, code int, message string) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	frame := make([]byte, 0, len(id)+len(message)+80)
	frame = append(frame, `{"jsonrpc":"2.0","id":`...)
	frame = append(frame, id...)
	frame = append(frame, `,"error":{"code":`...)
	frame = strconv.AppendInt(frame, int64(code), 10)
	frame = append(frame, `,"message":`...)
	frame = strconv.AppendQuote(frame, message)
	frame = append(frame, "}}"...)
	return frame
}

func compareCanonicalNumbers(left, right string) int {
	leftSign, leftDigits, leftExponent := splitCanonicalNumber(left)
	rightSign, rightDigits, rightExponent := splitCanonicalNumber(right)
	if leftSign != rightSign {
		if leftSign < rightSign {
			return -1
		}
		return 1
	}
	if leftSign == 0 {
		return 0
	}

	leftMagnitude := new(big.Int).Add(big.NewInt(int64(len(leftDigits))), leftExponent)
	rightMagnitude := new(big.Int).Add(big.NewInt(int64(len(rightDigits))), rightExponent)
	comparison := leftMagnitude.Cmp(rightMagnitude)
	if comparison == 0 {
		comparison = compareAlignedDigits(leftDigits, rightDigits)
	}
	if leftSign < 0 {
		return -comparison
	}
	return comparison
}

func splitCanonicalNumber(value string) (int, string, *big.Int) {
	if value == "0" {
		return 0, "", new(big.Int)
	}
	sign := 1
	if value[0] == '-' {
		sign = -1
		value = value[1:]
	}
	index := strings.LastIndexByte(value, 'e')
	exponent := new(big.Int)
	exponent.SetString(value[index+1:], 10)
	return sign, value[:index], exponent
}

func compareAlignedDigits(left, right string) int {
	length := len(left)
	if len(right) > length {
		length = len(right)
	}
	for index := range length {
		leftByte := byte('0')
		if index < len(left) {
			leftByte = left[index]
		}
		rightByte := byte('0')
		if index < len(right) {
			rightByte = right[index]
		}
		if leftByte < rightByte {
			return -1
		}
		if leftByte > rightByte {
			return 1
		}
	}
	return 0
}

func taskOperationResponse(record *requestRecord, response *parsedFrame) (*taskMeta, bool, error) {
	if record == nil || record.taskID == "" {
		return nil, false, nil
	}
	switch record.method {
	case "tasks/get", "tasks/cancel":
		if response.kind == frameError {
			return nil, true, nil
		}
		if response.kind != frameResult {
			return nil, true, fmt.Errorf("task operation requires a response")
		}
		meta, err := parseTaskMeta(response.result)
		if err != nil {
			return nil, true, fmt.Errorf("task operation result: %w", err)
		}
		if meta.id != record.taskID {
			return nil, true, fmt.Errorf("task operation result taskId mismatch")
		}
		if record.method == "tasks/cancel" && meta.status != "cancelled" {
			return nil, true, fmt.Errorf("tasks/cancel result must be cancelled")
		}
		return meta, true, nil
	case "tasks/result":
		if response.kind == frameError {
			return nil, true, nil
		}
		if response.kind != frameResult {
			return nil, true, fmt.Errorf("task result requires a response")
		}
		return &taskMeta{id: record.taskID, status: "completed"}, true, nil
	default:
		return nil, false, nil
	}
}

func taskRequestSupported(initialize []byte, response bool, method string) bool {
	frame, err := parseFrame(initialize, 0)
	if err != nil {
		return false
	}
	var root json.RawMessage
	if response {
		if frame.kind != frameResult {
			return false
		}
		root = frame.result
	} else {
		if frame.kind != frameRequest || frame.method != "initialize" {
			return false
		}
		root = frame.params
	}
	fields, err := decodeObject(root)
	if err != nil {
		return false
	}
	capabilitiesRaw, ok := fields["capabilities"]
	if !ok {
		return false
	}
	capabilities, err := decodeObject(capabilitiesRaw)
	if err != nil {
		return false
	}
	tasksRaw, ok := capabilities["tasks"]
	if !ok {
		return false
	}
	tasks, err := decodeObject(tasksRaw)
	if err != nil {
		return false
	}
	requestsRaw, ok := tasks["requests"]
	if !ok {
		return false
	}
	requests, err := decodeObject(requestsRaw)
	if err != nil {
		return false
	}
	categoryName, requestName, ok := strings.Cut(method, "/")
	if !ok || categoryName == "" || requestName == "" || strings.Contains(requestName, "/") {
		return false
	}
	categoryRaw, ok := requests[categoryName]
	if !ok {
		return false
	}
	category, err := decodeObject(categoryRaw)
	if err != nil {
		return false
	}
	capabilityRaw, ok := category[requestName]
	if !ok {
		return false
	}
	_, err = decodeObject(capabilityRaw)
	return err == nil
}

func supportsListChanged(initializeResponse []byte, kind ListChangedKind) bool {
	frame, err := parseFrame(initializeResponse, 0)
	if err != nil || frame.kind != frameResult {
		return false
	}
	result, err := decodeObject(frame.result)
	if err != nil {
		return false
	}
	capabilitiesRaw, ok := result["capabilities"]
	if !ok {
		return false
	}
	capabilities, err := decodeObject(capabilitiesRaw)
	if err != nil {
		return false
	}
	name := ""
	switch kind {
	case ListChangedTools:
		name = "tools"
	case ListChangedPrompts:
		name = "prompts"
	case ListChangedResources:
		name = "resources"
	default:
		return false
	}
	capabilityRaw, ok := capabilities[name]
	if !ok {
		return false
	}
	capability, err := decodeObject(capabilityRaw)
	if err != nil {
		return false
	}
	listChangedRaw, ok := capability["listChanged"]
	return ok && bytes.Equal(bytes.TrimSpace(listChangedRaw), []byte("true"))
}

func listChangedFrame(kind ListChangedKind) []byte {
	var method string
	switch kind {
	case ListChangedTools:
		method = "notifications/tools/list_changed"
	case ListChangedPrompts:
		method = "notifications/prompts/list_changed"
	case ListChangedResources:
		method = "notifications/resources/list_changed"
	default:
		return nil
	}
	return []byte(`{"jsonrpc":"2.0","method":"` + method + `"}`)
}
