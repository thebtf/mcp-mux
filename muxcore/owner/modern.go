package owner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/jsonrpc"
	"github.com/thebtf/mcp-mux/muxcore/remap"
	"github.com/thebtf/mcp-mux/muxcore/upstream"
)

const (
	modernSubscriptionID = "io.modelcontextprotocol/subscriptionId"
	modernLogLevel       = "io.modelcontextprotocol/logLevel"
)

var errModernRouteUnavailable = errors.New("modern request is no longer admitted or its correlation ID is already active")

// Only this record chooses modern recipients and claims terminal ownership.
// inflightTracker is a status mirror; the legacy progress/remap tables are unused.
type modernRequest struct {
	mu               sync.Mutex
	writeDone        chan struct{}
	writeSettled     bool
	ended            bool
	acknowledged     bool
	session          *Session
	process          *upstream.Process
	inflight         InflightRequest
	hostKey          string
	wireKey          string
	originalID       json.RawMessage
	wireID           json.RawMessage
	originalProgress json.RawMessage
	progressKey      string
	requested        modernNotificationFilter
	accepted         modernNotificationFilter
}

func (r *modernRequest) releaseWriteLocked() {
	if !r.writeSettled {
		r.writeSettled = true
		close(r.writeDone)
	}
}

type modernNotificationFilter struct {
	ToolsListChanged     bool     `json:"toolsListChanged"`
	PromptsListChanged   bool     `json:"promptsListChanged"`
	ResourcesListChanged bool     `json:"resourcesListChanged"`
	Resources            []string `json:"resourceSubscriptions"`
}

func (f modernNotificationFilter) includesURI(uri string) bool {
	for _, subscribed := range f.Resources {
		if uri == subscribed {
			return true
		}
	}
	return false
}

func (f modernNotificationFilter) allows(method, uri string) bool {
	switch method {
	case "notifications/tools/list_changed":
		return f.ToolsListChanged
	case "notifications/prompts/list_changed":
		return f.PromptsListChanged
	case "notifications/resources/list_changed":
		return f.ResourcesListChanged
	case "notifications/resources/updated":
		return f.includesURI(uri)
	default:
		return false
	}
}

func (f modernNotificationFilter) subsetOf(requested modernNotificationFilter) bool {
	if f.ToolsListChanged && !requested.ToolsListChanged ||
		f.PromptsListChanged && !requested.PromptsListChanged ||
		f.ResourcesListChanged && !requested.ResourcesListChanged {
		return false
	}
	for _, uri := range f.Resources {
		if !requested.includesURI(uri) {
			return false
		}
	}
	return true
}

func modernHasLogLevel(raw []byte) bool {
	if !bytes.Contains(raw, []byte(modernLogLevel)) {
		return false
	}
	var request struct {
		Params struct {
			Meta struct {
				LogLevel json.RawMessage `json:"io.modelcontextprotocol/logLevel"`
			} `json:"_meta"`
		} `json:"params"`
	}
	if json.Unmarshal(raw, &request) != nil {
		return false
	}
	return request.Params.Meta.LogLevel != nil
}

func (o *Owner) rejectModernSharedLogging(s *Session, id json.RawMessage) error {
	payload, err := buildJSONRPCErrorBytes(id, -32602, "request-scoped logLevel is not supported by a shared modern owner")
	if err != nil {
		return err
	}
	return s.WriteRaw(payload)
}

func (o *Owner) prepareModernRequest(s *Session, msg *jsonrpc.Message) (*modernRequest, []byte, error) {
	var request struct {
		Params struct {
			Meta struct {
				ProgressToken json.RawMessage `json:"progressToken"`
				LogLevel      json.RawMessage `json:"io.modelcontextprotocol/logLevel"`
			} `json:"_meta"`
			Notifications modernNotificationFilter `json:"notifications"`
		} `json:"params"`
	}
	if err := json.Unmarshal(msg.Raw, &request); err != nil {
		return nil, nil, err
	}
	var level string
	_ = json.Unmarshal(request.Params.Meta.LogLevel, &level)
	r := &modernRequest{
		session:   s,
		writeDone: make(chan struct{}),
		inflight: InflightRequest{
			Method: msg.Method, Tool: extractToolName(msg.Raw), SessionID: s.ID,
			StartTime: time.Now(), logSubscribed: level != "" && level != "off",
		},
		hostKey: string(remap.Remap(s.ID, msg.ID)), originalID: msg.ID, wireID: msg.ID,
		originalProgress: request.Params.Meta.ProgressToken, requested: request.Params.Notifications,
	}
	wireProgress := r.originalProgress
	if o.modernShared {
		r.wireID = json.RawMessage(strconv.Quote(o.proactiveNamespace + "-modern-" + strconv.FormatUint(o.modernSequence.Add(1), 10)))
		if r.originalProgress != nil {
			wireProgress = r.wireID
		}
	}
	r.wireKey = string(r.wireID)
	if wireProgress != nil {
		if o.modernShared {
			r.progressKey = r.wireKey
		} else {
			r.progressKey = string(wireProgress)
		}
	}
	if !o.modernShared {
		return r, msg.Raw, nil
	}
	raw, err := rewriteModernMessage(msg.Raw, r.wireID, "params", modernField{"progressToken", wireProgress, true})
	return r, raw, err
}

// Publication precedes transport I/O, which must never hold a retirement lock.
func (o *Owner) publishModernRequest(r *modernRequest, proc *upstream.Process) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	o.upstreamEventMu.RLock()
	defer o.upstreamEventMu.RUnlock()
	if proc != nil && !o.acceptsProcessEvent(proc) {
		return errModernRouteUnavailable
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.sessions[r.session.ID] != r.session || r.session.IsClosed() {
		return errModernRouteUnavailable
	}
	o.modernMu.Lock()
	defer o.modernMu.Unlock()
	if o.modernHostRequests[r.hostKey] != nil || o.modernRequests[r.wireKey] != nil ||
		(r.progressKey != "" && o.modernProgress[r.progressKey] != nil) {
		return errModernRouteUnavailable
	}
	if o.modernRequests == nil {
		o.modernRequests = make(map[string]*modernRequest)
		o.modernHostRequests = make(map[string]*modernRequest)
		o.modernProgress = make(map[string]*modernRequest)
	}
	r.process = proc
	r.inflight.process.Store(proc)
	o.modernRequests[r.wireKey] = r
	o.modernHostRequests[r.hostKey] = r
	if r.progressKey != "" {
		o.modernProgress[r.progressKey] = r
	}
	o.pendingRequests.Add(1)
	o.inflightTracker.Store(r.wireKey, &r.inflight)
	return nil
}

// r.mu serializes delivery with every terminal path, including detach and loss.
func (o *Owner) finishModernRequest(r *modernRequest) bool {
	if r.ended {
		return false
	}
	r.ended = true
	r.releaseWriteLocked()
	o.modernMu.Lock()
	delete(o.modernRequests, r.wireKey)
	delete(o.modernHostRequests, r.hostKey)
	if r.progressKey != "" {
		delete(o.modernProgress, r.progressKey)
	}
	o.modernMu.Unlock()
	o.inflightTracker.CompareAndDelete(r.wireKey, &r.inflight)
	o.decrementPending()
	return true
}

func (o *Owner) modernRequestByWire(key string, progress bool) *modernRequest {
	o.modernMu.Lock()
	defer o.modernMu.Unlock()
	if progress {
		return o.modernProgress[key]
	}
	return o.modernRequests[key]
}

func (o *Owner) modernSessionLive(r *modernRequest) bool {
	o.mu.RLock()
	live := o.sessions[r.session.ID] == r.session && !r.session.IsClosed()
	o.mu.RUnlock()
	return live
}

func (o *Owner) handleModernUpstreamResponse(proc *upstream.Process, msg *jsonrpc.Message) error {
	r := o.modernRequestByWire(string(msg.ID), false)
	if r == nil || r.process != proc {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ended {
		return nil
	}
	o.finishModernRequest(r)
	var response struct {
		Result struct {
			Meta struct {
				SubscriptionID json.RawMessage `json:"io.modelcontextprotocol/subscriptionId"`
			} `json:"_meta"`
		} `json:"result"`
	}
	var subscription json.RawMessage
	if r.inflight.Method == "subscriptions/listen" {
		if err := json.Unmarshal(msg.Raw, &response); err != nil {
			return o.writeDemandError(r.session, r.originalID, "invalid upstream subscription response")
		}
		subscription = response.Result.Meta.SubscriptionID
		if subscription != nil && !bytes.Equal(subscription, r.wireID) {
			return o.writeDemandError(r.session, r.originalID, "upstream subscription correlation mismatch")
		}
	}
	raw := msg.Raw
	if o.modernShared {
		var replacement json.RawMessage
		if subscription != nil {
			replacement = r.originalID
		}
		var err error
		raw, err = rewriteModernMessage(raw, r.originalID, "result", modernField{modernSubscriptionID, replacement, true})
		if err != nil {
			return err
		}
	}
	if !o.modernSessionLive(r) {
		return nil
	}
	return r.session.WriteRaw(raw)
}

func (o *Owner) handleModernUpstreamNotification(proc *upstream.Process, msg *jsonrpc.Message) error {
	if msg.Method == "notifications/message" {
		return o.routeModernLog(proc, msg.Raw)
	}
	var notification struct {
		Params struct {
			Meta struct {
				SubscriptionID json.RawMessage `json:"io.modelcontextprotocol/subscriptionId"`
			} `json:"_meta"`
			ProgressToken json.RawMessage          `json:"progressToken"`
			RequestID     json.RawMessage          `json:"requestId"`
			Notifications modernNotificationFilter `json:"notifications"`
			URI           string                   `json:"uri"`
		} `json:"params"`
	}
	if err := json.Unmarshal(msg.Raw, &notification); err != nil {
		return err
	}
	p := &notification.Params
	key := p.Meta.SubscriptionID
	progress := msg.Method == "notifications/progress"
	cancelled := msg.Method == "notifications/cancelled"
	if progress {
		if key != nil {
			return nil
		}
		key = p.ProgressToken
	} else if cancelled {
		key = p.RequestID
	}
	if key == nil {
		return nil
	}
	r := o.modernRequestByWire(string(key), progress)
	if r == nil || r.process != proc {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ended || !o.modernSessionLive(r) {
		return nil
	}
	field, meta, replacement := modernSubscriptionID, true, r.originalID
	switch {
	case progress:
		field, meta, replacement = "progressToken", false, r.originalProgress
	case r.inflight.Method != "subscriptions/listen":
		return nil
	case cancelled:
		if subscription := p.Meta.SubscriptionID; subscription != nil && !bytes.Equal(subscription, r.wireID) {
			return nil
		}
		field, meta = "requestId", false
	case msg.Method == "notifications/subscriptions/acknowledged":
		if r.acknowledged || !p.Notifications.subsetOf(r.requested) {
			return nil
		}
		r.accepted = p.Notifications
		r.acknowledged = true
	default:
		if !r.acknowledged || !r.requested.allows(msg.Method, p.URI) || !r.accepted.allows(msg.Method, p.URI) {
			return nil
		}
	}
	raw := msg.Raw
	if o.modernShared {
		var subscription json.RawMessage
		if cancelled && p.Meta.SubscriptionID != nil {
			subscription = r.originalID
		}
		var err error
		raw, err = rewriteModernMessage(raw, nil, "params", modernField{field, replacement, meta}, modernField{modernSubscriptionID, subscription, true})
		if err != nil {
			return err
		}
	}
	if cancelled {
		o.finishModernRequest(r)
	}
	return r.session.WriteRaw(raw)
}

func (o *Owner) routeModernLog(proc *upstream.Process, raw []byte) error {
	if o.modernShared {
		return nil
	}
	o.modernMu.Lock()
	var recipient *modernRequest
	for _, r := range o.modernRequests {
		if r.process != proc || !r.inflight.logSubscribed {
			continue
		}
		if recipient != nil && recipient.session != r.session {
			o.modernMu.Unlock()
			return nil
		}
		recipient = r
	}
	o.modernMu.Unlock()
	if recipient == nil {
		return nil
	}
	recipient.mu.Lock()
	defer recipient.mu.Unlock()
	if recipient.ended || !o.modernSessionLive(recipient) {
		return nil
	}
	return recipient.session.WriteRaw(raw)
}

func (o *Owner) forwardModernCancelledNotification(s *Session, msg *jsonrpc.Message) error {
	var notification struct {
		Params struct {
			RequestID json.RawMessage `json:"requestId"`
			Meta      struct {
				SubscriptionID json.RawMessage `json:"io.modelcontextprotocol/subscriptionId"`
			} `json:"_meta"`
		} `json:"params"`
	}
	if json.Unmarshal(msg.Raw, &notification) != nil || notification.Params.RequestID == nil {
		return nil
	}
	if o.cancelLocalDemand(s, notification.Params.RequestID) || o.cancelActiveSessionRequest(s, notification.Params.RequestID) {
		return nil
	}
	o.modernMu.Lock()
	r := o.modernHostRequests[string(remap.Remap(s.ID, notification.Params.RequestID))]
	o.modernMu.Unlock()
	if r == nil || r.session != s {
		return nil
	}
	select {
	case <-r.writeDone:
	case <-s.Done():
		return nil
	case <-o.materializationStop:
		return nil
	}
	raw, err := func() ([]byte, error) {
		o.upstreamEventMu.RLock()
		defer o.upstreamEventMu.RUnlock()
		if r.process != nil && !o.acceptsProcessEvent(r.process) {
			return nil, nil
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.ended || !o.modernSessionLive(r) {
			return nil, nil
		}
		raw := msg.Raw
		if o.modernShared {
			var subscription json.RawMessage
			if id := notification.Params.Meta.SubscriptionID; id != nil {
				if string(remap.Remap(s.ID, id)) != r.hostKey {
					return nil, nil
				}
				subscription = r.wireID
			}
			var err error
			raw, err = rewriteModernMessage(raw, nil, "params", modernField{"requestId", r.wireID, false}, modernField{modernSubscriptionID, subscription, true})
			if err != nil {
				return nil, err
			}
		}
		o.finishModernRequest(r)
		return raw, nil
	}()
	if err != nil || raw == nil {
		return err
	}
	if r.process != nil {
		return o.writeUpstreamToProcess(r.process, raw)
	}
	if o.upstreamWriter != nil {
		return o.writeUpstream(raw)
	}
	return nil
}

func (o *Owner) modernRequestsFor(proc *upstream.Process, session *Session) []*modernRequest {
	o.modernMu.Lock()
	defer o.modernMu.Unlock()
	var requests []*modernRequest
	for _, r := range o.modernRequests {
		if (proc == nil || r.process == proc) && (session == nil || r.session == session) {
			requests = append(requests, r)
		}
	}
	return requests
}

func (o *Owner) claimModernInflightRequests(proc *upstream.Process) []drainedInflightResponse {
	var responses []drainedInflightResponse
	for _, r := range o.modernRequestsFor(proc, nil) {
		r.mu.Lock()
		if !o.finishModernRequest(r) {
			r.mu.Unlock()
			continue
		}
		if o.modernSessionLive(r) {
			payload, err := buildJSONRPCErrorBytes(r.originalID, -32603, "upstream process exited")
			if held := o.maintenance.Load(); held != nil {
				payload = maintenanceErrorBytes(r.originalID, held)
			}
			if err == nil {
				responses = append(responses, drainedInflightResponse{session: r.session, sessionID: r.session.ID, payload: payload})
			}
		}
		r.mu.Unlock()
	}
	return responses
}

func (o *Owner) removeModernInflightForSession(session *Session) {
	for _, r := range o.modernRequestsFor(nil, session) {
		r.mu.Lock()
		o.finishModernRequest(r)
		r.mu.Unlock()
	}
}

type modernField struct {
	name  string
	value json.RawMessage
	meta  bool
}

// Rewrite correlation fields in one envelope pass without interpreting MRTR state.
func rewriteModernMessage(raw []byte, id json.RawMessage, section string, fields ...modernField) ([]byte, error) {
	changed := false
	for _, field := range fields {
		changed = changed || field.value != nil
	}
	if !changed {
		if id == nil {
			return raw, nil
		}
		return jsonrpc.ReplaceID(raw, id)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(envelope[section], &body); err != nil || body == nil {
		return nil, fmt.Errorf("invalid modern %s correlation object", section)
	}
	var metadata map[string]json.RawMessage
	for _, field := range fields {
		if field.value == nil {
			continue
		}
		if field.meta {
			if metadata == nil {
				if err := json.Unmarshal(body["_meta"], &metadata); err != nil || metadata == nil {
					return nil, fmt.Errorf("invalid modern %s metadata", section)
				}
			}
			metadata[field.name] = field.value
		} else {
			body[field.name] = field.value
		}
	}
	if metadata != nil {
		encoded, err := json.Marshal(metadata)
		if err != nil {
			return nil, err
		}
		body["_meta"] = encoded
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	envelope[section] = encoded
	if id != nil {
		envelope["id"] = id
	}
	return json.Marshal(envelope)
}
