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
	initialize    bool
	generation    uint64
	progressToken *scalarValue
	taskAugmented bool
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
)

type tombstoneKey struct {
	role tombstoneRole
	key  scalarKey
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
	requests   map[scalarKey]*requestRecord
	tokens     map[scalarKey]*tokenRecord
	tasks      map[string]*taskRecord
	tombstones tombstoneSet
}

func newDirectionRegistry() directionRegistry {
	return directionRegistry{
		requests: make(map[scalarKey]*requestRecord),
		tokens:   make(map[scalarKey]*tokenRecord),
		tasks:    make(map[string]*taskRecord),
	}
}

func (registry *directionRegistry) canRegister(frame *parsedFrame) error {
	if frame.id == nil {
		return fmt.Errorf("request id is missing")
	}
	if len(registry.requests)+len(registry.tasks) >= activeCorrelationLimit {
		return fmt.Errorf("active correlation limit reached")
	}
	if registry.tombstones.contains(tombstoneRequest, frame.id.key) {
		return fmt.Errorf("request id is tombstoned")
	}
	if _, exists := registry.requests[frame.id.key]; exists {
		return fmt.Errorf("duplicate active request id")
	}
	if frame.progressToken != nil {
		if registry.tombstones.contains(tombstoneToken, frame.progressToken.key) {
			return fmt.Errorf("progress token is tombstoned")
		}
		if _, exists := registry.tokens[frame.progressToken.key]; exists {
			return fmt.Errorf("duplicate active progress token")
		}
	}
	return nil
}

func (registry *directionRegistry) register(frame *parsedFrame, generation uint64) *requestRecord {
	record := &requestRecord{
		id:            *frame.id,
		initialize:    frame.method == "initialize",
		generation:    generation,
		progressToken: frame.progressToken,
		taskAugmented: frame.taskAugmented,
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

func (registry *directionRegistry) canComplete(record *requestRecord, response *parsedFrame) error {
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
	if meta.status != "working" && meta.status != "input_required" {
		return nil
	}
	if existing := registry.tasks[meta.id]; existing != nil && existing.requestID != record.id.key {
		return fmt.Errorf("duplicate active taskId")
	}
	return nil
}

func (registry *directionRegistry) complete(record *requestRecord, response *parsedFrame) {
	delete(registry.requests, record.id.key)
	registry.tombstones.add(tombstoneRequest, record.id.key)

	if record.taskAugmented && response.kind == frameResult && response.taskResult != nil {
		meta := response.taskResult
		if meta.status == "working" || meta.status == "input_required" {
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
	delete(registry.tasks, meta.id)
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
	for key, record := range registry.requests {
		if record.generation != generation {
			continue
		}
		delete(registry.requests, key)
		registry.tombstones.add(tombstoneRequest, key)
		registry.removeToken(record)
		if !record.initialize {
			lost = append(lost, record)
		}
	}
	for id, task := range registry.tasks {
		if task.generation != generation {
			continue
		}
		delete(registry.tasks, id)
		if task.progressToken != nil {
			delete(registry.tokens, *task.progressToken)
			registry.tombstones.add(tombstoneToken, *task.progressToken)
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

func (registry *directionRegistry) clearTransitionTombstones() {
	registry.tombstones.clear()
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
