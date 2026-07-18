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

const (
	tombstoneLimit           = 1024
	activeCorrelationLimit   = 256
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

type directionRegistry struct {
	requests          map[scalarKey]*requestRecord
	tokens            map[scalarKey]*tokenRecord
	tasks             map[string]*taskRecord
	tombstones        tombstoneSet
	retiredTombstones tombstoneSet
	retiredRequests   map[scalarKey]*requestRecord
	retiredTokens     map[scalarKey]struct{}
	retiredTasks      map[string]*taskRecord
}

func newDirectionRegistry() directionRegistry {
	return directionRegistry{
		requests:        make(map[scalarKey]*requestRecord),
		tokens:          make(map[scalarKey]*tokenRecord),
		tasks:           make(map[string]*taskRecord),
		retiredRequests: make(map[scalarKey]*requestRecord),
		retiredTokens:   make(map[scalarKey]struct{}),
		retiredTasks:    make(map[string]*taskRecord),
	}
}

func (registry *directionRegistry) canRegister(frame *parsedFrame) error {
	if frame.id == nil {
		return fmt.Errorf("request id is missing")
	}
	if _, exists := registry.retiredRequests[frame.id.key]; exists {
		return fmt.Errorf("request id belongs to a retired generation")
	}
	if registry.tombstones.contains(tombstoneRequest, frame.id.key) || registry.retiredTombstones.contains(tombstoneRequest, frame.id.key) {
		return fmt.Errorf("request id is tombstoned")
	}
	if _, exists := registry.requests[frame.id.key]; exists {
		return fmt.Errorf("duplicate active request id")
	}
	if len(registry.requests)+len(registry.retiredRequests) >= activeCorrelationLimit {
		return fmt.Errorf("request correlation limit reached")
	}
	if frame.progressToken != nil {
		if _, exists := registry.retiredTokens[frame.progressToken.key]; exists {
			return fmt.Errorf("progress token belongs to a retired generation")
		}
		if registry.tombstones.contains(tombstoneToken, frame.progressToken.key) || registry.retiredTombstones.contains(tombstoneToken, frame.progressToken.key) {
			return fmt.Errorf("progress token is tombstoned")
		}
		if _, exists := registry.tokens[frame.progressToken.key]; exists {
			return fmt.Errorf("duplicate active progress token")
		}
		if len(registry.tokens)+len(registry.retiredTokens) >= activeCorrelationLimit {
			return fmt.Errorf("progress token correlation limit reached")
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
	if _, exists := registry.retiredTasks[taskID]; exists {
		return fmt.Errorf("taskId belongs to a retired generation")
	}
	if registry.tombstones.contains(tombstoneTask, taskTombstoneKey(taskID)) || registry.retiredTombstones.contains(tombstoneTask, taskTombstoneKey(taskID)) {
		return fmt.Errorf("taskId is tombstoned")
	}
	if len(registry.tasks)+len(registry.retiredTasks) >= activeCorrelationLimit {
		return fmt.Errorf("task correlation limit reached")
	}
	return nil
}

func (registry *directionRegistry) canComplete(record *requestRecord, response *parsedFrame) error {
	if _, handled, err := taskOperationResponse(record, response); handled {
		return err
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

func (registry *directionRegistry) complete(record *requestRecord, response *parsedFrame) {
	delete(registry.requests, record.id.key)
	registry.tombstones.add(tombstoneRequest, record.id.key)

	if meta, handled, _ := taskOperationResponse(record, response); handled {
		if meta != nil {
			if meta.status == "working" || meta.status == "input_required" {
				if task := registry.tasks[meta.id]; task != nil && task.generation == record.generation {
					task.status = meta.status
				}
			} else {
				registry.retireTask(meta.id, record.generation)
			}
		}
		registry.removeToken(record)
		return
	}

	if record.taskAugmented && response.kind == frameResult && response.taskResult != nil {
		meta := response.taskResult
		if meta.status == "working" {
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
			return
		}
	}
	registry.removeToken(record)
}

func (registry *directionRegistry) cancel(record *requestRecord) {
	delete(registry.requests, record.id.key)
	registry.tombstones.add(tombstoneRequest, record.id.key)
	registry.removeToken(record)
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
	if record.progressToken == nil {
		return
	}
	delete(registry.tokens, record.progressToken.key)
	registry.tombstones.add(tombstoneToken, record.progressToken.key)
}

func (registry *directionRegistry) updateTask(meta *taskMeta, generation uint64) bool {
	task := registry.tasks[meta.id]
	if task == nil || task.generation != generation {
		return false
	}
	task.status = meta.status
	if meta.status == "working" || meta.status == "input_required" {
		return true
	}
	return registry.retireTask(meta.id, generation)
}

func (registry *directionRegistry) retireTask(taskID string, generation uint64) bool {
	task := registry.tasks[taskID]
	if task == nil || task.generation != generation {
		return false
	}
	delete(registry.tasks, taskID)
	registry.tombstones.add(tombstoneTask, taskTombstoneKey(taskID))
	if task.progressToken != nil {
		delete(registry.tokens, *task.progressToken)
		registry.tombstones.add(tombstoneToken, *task.progressToken)
	}
	return true
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

func (registry *directionRegistry) retireGeneration(generation uint64) []*requestRecord {
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
		if task.requestID.kind != 0 {
			registry.retiredTombstones.add(tombstoneRequest, task.requestID)
		}
		if task.progressToken != nil {
			delete(registry.tokens, *task.progressToken)
			registry.retiredTokens[*task.progressToken] = struct{}{}
		}
	}
	sort.Slice(lost, func(i, j int) bool {
		if lost[i].id.key.kind != lost[j].id.key.kind {
			return lost[i].id.key.kind < lost[j].id.key.kind
		}
		return lost[i].id.key.value < lost[j].id.key.value
	})
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
	if !record.taskAugmented || response.kind == frameError {
		registry.releaseRetiredRequest(record)
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
	registry.retiredTombstones.add(tombstoneRequest, record.id.key)
	registry.retiredTasks[meta.id] = task
	return true, nil
}

func (registry *directionRegistry) releaseRetiredRequest(record *requestRecord) {
	delete(registry.retiredRequests, record.id.key)
	tombstones := &registry.tombstones
	if record.taskAugmented {
		tombstones = &registry.retiredTombstones
	}
	tombstones.add(tombstoneRequest, record.id.key)
	if record.progressToken != nil {
		delete(registry.retiredTokens, record.progressToken.key)
		tombstones.add(tombstoneToken, record.progressToken.key)
	}
}

func (registry *directionRegistry) consumeRetiredTaskStatus(meta *taskMeta) (bool, error) {
	if meta == nil {
		return false, nil
	}
	taskKey := taskTombstoneKey(meta.id)
	task := registry.retiredTasks[meta.id]
	if task == nil {
		if !registry.retiredTombstones.contains(tombstoneTask, taskKey) {
			return false, nil
		}
		if _, exists := registry.tasks[meta.id]; exists {
			return true, fmt.Errorf("taskId belongs to an active task and a retired tombstone")
		}
		return true, nil
	}
	if _, exists := registry.tasks[meta.id]; exists {
		return true, fmt.Errorf("taskId belongs to active and retired generations")
	}
	if registry.tombstones.contains(tombstoneTask, taskKey) || registry.retiredTombstones.contains(tombstoneTask, taskKey) {
		return true, fmt.Errorf("retired taskId is also tombstoned")
	}
	task.status = meta.status
	if meta.status == "working" || meta.status == "input_required" {
		return true, nil
	}
	delete(registry.retiredTasks, meta.id)
	registry.retiredTombstones.add(tombstoneTask, taskKey)
	if task.progressToken != nil {
		delete(registry.retiredTokens, *task.progressToken)
		registry.retiredTombstones.add(tombstoneToken, *task.progressToken)
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
	registry.retiredTombstones.clear()
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
