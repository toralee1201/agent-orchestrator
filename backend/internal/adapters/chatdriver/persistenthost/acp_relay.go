package persistenthost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
)

const (
	// ACPEventIDMetaKey identifies one replayable provider event. AO assigns the
	// value to every durable event derived from that frame.
	ACPEventIDMetaKey = "ao.persistentEventId"

	// ACPPromptStalledDataKey marks the error data of a prompt the stall
	// watchdog failed. The daemon reads it to tell a silent adapter from a
	// provider that answered with an error.
	ACPPromptStalledDataKey = "ao.promptStalled"

	// ACPPromptStallTimeoutEnv overrides promptStallTimeout. It is a Go duration
	// strictly greater than zero; an unset, invalid, or non-positive value falls
	// back to promptStallTimeout.
	ACPPromptStallTimeoutEnv = "AO_ACP_PROMPT_STALL_TIMEOUT"

	// promptStallTimeout is the default silence budget for an in-flight
	// session/prompt. It is also the value used when ACPPromptStallTimeoutEnv is
	// unset or invalid. A live turn streams tool calls, message chunks, or state
	// changes continuously, so a silent gap this long means the adapter's turn
	// state machine lost the completion (upstream claude-agent-acp#1145 class)
	// and no response will ever arrive. The effective per-relay value lives in
	// acpRelay.promptStallTimeout and may be configured at process start.
	promptStallTimeout = 15 * time.Minute

	// toolCallStallTimeout is the hard silence cap while the adapter has an
	// active tool call. A build or test command can legitimately run longer than
	// promptStallTimeout with no output at all, so silence alone must not settle
	// such a turn at the base deadline. This bound still guarantees a dead
	// adapter is eventually caught.
	toolCallStallTimeout = 30 * time.Minute

	// promptStallCheckInterval is how often the host re-checks for a stalled
	// prompt.
	promptStallCheckInterval = 30 * time.Second

	// promptStallCancelGrace is how long the relay waits, after cancelling a
	// stalled prompt, for the adapter to answer it before giving up on the
	// adapter and settling the prompt itself.
	promptStallCancelGrace = 30 * time.Second
)

// abandonedPrompt records a session/prompt the watchdog settled with a
// synthetic failure after the cancel grace expired. The adapter's late answer
// to it is discarded, not attributed to a later prompt. settledAt is the real
// time the failure was emitted (not a backdated guess) and silence is the
// window that triggered the stall, so the discard log reports accurate values.
type abandonedPrompt struct {
	settledAt time.Time
	silence   time.Duration
}

type acpClientRequest struct {
	clientID   json.RawMessage
	method     string
	params     json.RawMessage
	generation uint64
}

type acpServerRequest struct {
	requestID   string
	frame       []byte
	promptOwned bool
}

type acpInteractionCommand struct {
	eventID     string
	canonical   string
	params      json.RawMessage
	promptOwned bool
}

type acpReplayKind uint8

const (
	acpReplayFrame acpReplayKind = iota
	acpReplayRequest
	acpReplayCommand
)

type acpReplayEntry struct {
	kind acpReplayKind
	key  string
	span acpJournalSpan
}

type acpClientFrames struct {
	provider []byte
	client   []byte
}

// acpRelay is deliberately only a JSON-RPC correlation layer. The pinned ACP
// SDK and all provider semantics remain in the adapter; the host owns only the
// connection state that cannot be reconstructed after a daemon dies.
type acpRelay struct {
	identity          string
	nextRequestID     int64
	nextEventID       uint64
	nextInteractionID uint64
	pending           map[string]acpClientRequest
	serverRequests    map[string]acpServerRequest
	serverOrder       []string
	commands          map[string]acpInteractionCommand
	commandOrder      []string
	promptReplay      []acpReplayEntry
	state             ACPState
	promptJournal     *acpPromptJournal
	promptResult      []byte
	// availableCommands holds the latest command catalog the adapter announced
	// and availableCommandsSession the session it belongs to; see
	// ACPState.AvailableCommands.
	availableCommands        json.RawMessage
	availableCommandsSession string
	cancelRequested          bool
	// lastActivityAt stamps the last adapter output, prompt registration, or
	// client answer to an adapter request, so the stall watchdog can tell a
	// live turn from a lost one.
	lastActivityAt time.Time
	// promptStallTimeout is the effective base silence budget for this host.
	// It defaults to promptStallTimeout and may be overridden by
	// ACPPromptStallTimeoutEnv at construction.
	promptStallTimeout time.Duration
	// activeToolCalls holds ids of tool calls the adapter announced but has not
	// reported as finished. While non-empty the watchdog uses the longer
	// toolCallStallTimeout instead of the base silence budget, so a build or
	// test command that emits nothing for a long time is not killed mid-run.
	activeToolCalls map[string]struct{}
	// stallCancelledAt is when the watchdog sent session/cancel to the active
	// prompt for silence. The prompt stays active until the adapter answers.
	stallCancelledAt time.Time
	// stallCancelledPromptID is the provider id of that cancelled prompt, so a
	// late answer can only be adopted for the exact prompt that was cancelled.
	stallCancelledPromptID string
	// abandonedPrompts holds provider ids of prompts whose synthetic stall
	// failure was already delivered to the controller. A late adapter answer to
	// one of these is discarded with a log, never attributed to the next prompt.
	abandonedPrompts map[string]abandonedPrompt
	// logger records watchdog decisions as structured events. It is never nil.
	logger *slog.Logger
}

type acpRelayOption func(*acpRelay)

// withACPRelayLogger injects the structured logger used by the stall watchdog.
func withACPRelayLogger(logger *slog.Logger) acpRelayOption {
	return func(r *acpRelay) {
		if logger != nil {
			r.logger = logger
		}
	}
}

func newACPRelay(ctx context.Context, journalPath string, opts ...acpRelayOption) (*acpRelay, error) {
	identity, err := randomToken()
	if err != nil {
		return nil, err
	}
	journal, err := openACPPromptJournal(ctx, journalPath)
	if err != nil {
		return nil, err
	}
	r := &acpRelay{
		identity:           identity,
		pending:            make(map[string]acpClientRequest),
		serverRequests:     make(map[string]acpServerRequest),
		commands:           make(map[string]acpInteractionCommand),
		activeToolCalls:    make(map[string]struct{}),
		abandonedPrompts:   make(map[string]abandonedPrompt),
		promptStallTimeout: loadPromptStallTimeout(),
		promptJournal:      journal,
		logger:             slog.New(slog.DiscardHandler),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

// loadPromptStallTimeout resolves ACPPromptStallTimeoutEnv following the daemon's
// duration-env convention: a positive Go duration overrides the default, while
// an unset, malformed, or non-positive value keeps the safe default.
func loadPromptStallTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv(ACPPromptStallTimeoutEnv))
	if raw == "" {
		return promptStallTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return promptStallTimeout
	}
	return d
}

// stallTimeout is the silence budget for the active prompt. A running tool
// call gets the longer hard cap: a build or test command may be silent far
// longer than the base budget, and it is still bounded so a wedged adapter is
// eventually caught.
func (r *acpRelay) stallTimeout() time.Duration {
	if len(r.activeToolCalls) > 0 && r.toolCallTimeout() > r.promptStallTimeout {
		return r.toolCallTimeout()
	}
	return r.promptStallTimeout
}

func (r *acpRelay) toolCallTimeout() time.Duration {
	// The hard cap is at least toolCallStallTimeout and grows with the base
	// budget so a custom long base also gives long tool calls room to finish.
	// It is never shorter than the base timeout.
	hardCap := toolCallStallTimeout
	if scaled := r.promptStallTimeout * 2; scaled > hardCap {
		hardCap = scaled
	}
	return hardCap
}

func (r *acpRelay) snapshot() *ACPState {
	state := r.state
	state.InitializeResult = append(json.RawMessage(nil), state.InitializeResult...)
	state.SessionResult = append(json.RawMessage(nil), state.SessionResult...)
	if r.availableCommandsSession != "" && r.availableCommandsSession == state.SessionID {
		state.AvailableCommands = append(json.RawMessage(nil), r.availableCommands...)
	}
	return &state
}

// captureAvailableCommands keeps the latest available_commands_update. ACP
// updates replace the whole catalog, so only the last one matters.
func (r *acpRelay) captureAvailableCommands(method string, params json.RawMessage) {
	if method != "session/update" {
		return
	}
	var notification struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate     string          `json:"sessionUpdate"`
			AvailableCommands json.RawMessage `json:"availableCommands"`
		} `json:"update"`
	}
	if json.Unmarshal(params, &notification) != nil ||
		notification.Update.SessionUpdate != "available_commands_update" ||
		notification.SessionID == "" {
		return
	}
	r.availableCommands = append(json.RawMessage(nil), notification.Update.AvailableCommands...)
	r.availableCommandsSession = notification.SessionID
}

// trackToolCall maintains the active tool-call set from ACP session/update
// events:
//
//   - tool_call starts a call (id + non-terminal status) and records it; a call
//     announced already terminal is never counted as active;
//   - tool_call_update with status in_progress/pending re-asserts it; completed,
//     failed, cancelled, or error ends it; an empty status is ignored so it
//     cannot resurrect a call that already finished.
//
// Only updates for the active session are counted, so an event from another
// session cannot dilute this turn's watchdog budget. Other session updates
// leave the set untouched. Permission requests are handled separately by
// serverRequests, so a call waiting on the user is not counted as adapter
// silence either way. The caller holds the host mutex.
func (r *acpRelay) trackToolCall(method string, params json.RawMessage) {
	if method != "session/update" || len(params) == 0 {
		return
	}
	var notification struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string `json:"sessionUpdate"`
			ToolCallID    string `json:"toolCallId"`
			Status        string `json:"status"`
		} `json:"update"`
	}
	if json.Unmarshal(params, &notification) != nil || notification.SessionID == "" {
		return
	}
	if active := r.state.SessionID; active != "" && notification.SessionID != active {
		return
	}
	switch notification.Update.SessionUpdate {
	case "tool_call":
		id := notification.Update.ToolCallID
		if id == "" || terminalToolCallStatus(notification.Update.Status) {
			return
		}
		r.activeToolCalls[id] = struct{}{}
	case "tool_call_update":
		id := notification.Update.ToolCallID
		if id == "" {
			return
		}
		switch strings.ToLower(notification.Update.Status) {
		case "completed", "failed", "cancelled", "error":
			delete(r.activeToolCalls, id)
		case "in_progress", "pending":
			r.activeToolCalls[id] = struct{}{}
		}
	}
}

// terminalToolCallStatus reports whether status names a finished tool call.
// An empty status is not terminal: ACP uses it for a freshly announced call,
// which starts in progress unless it explicitly says otherwise.
func terminalToolCallStatus(status string) bool {
	switch strings.ToLower(status) {
	case "completed", "failed", "cancelled", "error":
		return true
	default:
		return false
	}
}

func (r *acpRelay) replayTo(ctx context.Context, dst io.Writer) error {
	for _, entry := range r.promptReplay {
		switch entry.kind {
		case acpReplayFrame:
			if err := r.promptJournal.replayTo(ctx, dst, entry.span); err != nil {
				return err
			}
		case acpReplayRequest:
			if request, ok := r.serverRequest(entry.key); ok {
				if _, err := dst.Write(request.frame); err != nil {
					return err
				}
			}
		case acpReplayCommand:
			if command, ok := r.commands[entry.key]; ok {
				if err := r.replayInteractionCommand(ctx, dst, entry.key, command); err != nil {
					return err
				}
			}
		}
	}
	for _, providerID := range r.serverOrder {
		request, ok := r.serverRequests[providerID]
		if !ok || request.promptOwned {
			continue
		}
		if _, err := dst.Write(request.frame); err != nil {
			return err
		}
	}
	for _, requestID := range r.commandOrder {
		command, ok := r.commands[requestID]
		if !ok || command.promptOwned {
			continue
		}
		if err := r.replayInteractionCommand(ctx, dst, requestID, command); err != nil {
			return err
		}
	}
	if len(r.promptResult) > 0 {
		_, err := dst.Write(r.promptResult)
		return err
	}
	return nil
}

func (r *acpRelay) replayInteractionCommand(
	ctx context.Context,
	dst io.Writer,
	requestID string,
	command acpInteractionCommand,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(command.params, &params); err != nil {
		return err
	}
	params["eventId"], _ = json.Marshal(command.eventID)
	_, pending := r.serverRequest(requestID)
	params["providerPending"], _ = json.Marshal(pending)
	encoded, _ := json.Marshal(params)
	_, err := dst.Write(marshalFrame(map[string]json.RawMessage{
		"jsonrpc": json.RawMessage(`"2.0"`),
		"method":  json.RawMessage(`"` + ACPInteractionCommandMethod + `"`),
		"params":  encoded,
	}, nil))
	return err
}

func (r *acpRelay) close(ctx context.Context) error { return r.promptJournal.close(ctx) }

func (r *acpRelay) clientFrame(ctx context.Context, frame []byte, generation uint64) (acpClientFrames, error) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(frame, &envelope) != nil {
		return acpClientFrames{provider: frame}, nil //nolint:nilerr // preserve malformed input so the ACP peer owns its protocol error
	}
	var method string
	_ = json.Unmarshal(envelope["method"], &method)
	if method == ACPPromptAckMethod {
		var params struct {
			EventID string `json:"eventId"`
		}
		_ = json.Unmarshal(envelope["params"], &params)
		if params.EventID != "" && params.EventID == r.state.PendingResultEventID {
			if err := r.promptJournal.reset(ctx); err != nil {
				return acpClientFrames{}, err
			}
			r.promptResult = nil
			r.state.PendingResultEventID = ""
			r.commands = make(map[string]acpInteractionCommand)
			r.commandOrder = nil
			r.promptReplay = nil
		}
		return acpClientFrames{}, nil
	}
	if method == ACPInteractionCommandMethod {
		return r.acceptInteractionCommand(ctx, envelope)
	}
	if method == "$/cancel_request" {
		return acpClientFrames{provider: r.clientCancellation(envelope, frame, generation)}, nil
	}
	if method == "session/cancel" && r.state.ActivePrompt {
		if r.cancelRequested {
			return acpClientFrames{}, nil
		}
		r.cancelRequested = true
	}
	id := envelope["id"]
	if method == "" && len(id) > 0 {
		r.removeServerRequest(string(id))
		// The adapter resumes the turn once the client answers it.
		r.lastActivityAt = time.Now()
	}
	if method == "" || len(id) == 0 || string(id) == "null" {
		return acpClientFrames{provider: frame}, nil
	}

	if method == "session/prompt" && (r.state.ActivePrompt || r.state.PendingResultEventID != "") {
		return acpClientFrames{client: rpcErrorFrame(id, -32602, "previous ACP prompt is active or not acknowledged", nil)}, nil
	}
	r.nextRequestID++
	providerID, _ := json.Marshal(r.nextRequestID)
	r.pending[string(providerID)] = acpClientRequest{
		clientID: append(json.RawMessage(nil), id...), method: method,
		params: append(json.RawMessage(nil), envelope["params"]...), generation: generation,
	}
	envelope["id"] = providerID
	if method == "session/prompt" {
		r.state.ActivePrompt = true
		r.state.ActiveCompaction = isCompactionPrompt(envelope["params"])
		r.state.PendingResultEventID = ""
		r.cancelRequested = false
		r.lastActivityAt = time.Now()
		r.stallCancelledAt = time.Time{}
		r.stallCancelledPromptID = ""
		r.clearToolCalls()
		if err := r.promptJournal.reset(ctx); err != nil {
			return acpClientFrames{}, err
		}
		r.promptResult = nil
		r.commands = make(map[string]acpInteractionCommand)
		r.commandOrder = nil
		r.promptReplay = nil
	}
	return acpClientFrames{provider: marshalFrame(envelope, frame)}, nil
}

func (r *acpRelay) acceptInteractionCommand(
	ctx context.Context,
	envelope map[string]json.RawMessage,
) (acpClientFrames, error) {
	id := envelope["id"]
	var identity struct {
		RequestID string `json:"requestId"`
	}
	params := envelope["params"]
	if len(id) == 0 || string(id) == "null" || json.Unmarshal(params, &identity) != nil || identity.RequestID == "" {
		return acpClientFrames{client: rpcErrorFrame(id, -32602, "invalid persistent interaction command", nil)}, nil //nolint:nilerr // protocol validation errors are returned in-band
	}
	canonical, err := canonicalJSON(params)
	if err != nil {
		return acpClientFrames{client: rpcErrorFrame(id, -32602, "invalid persistent interaction command", nil)}, nil //nolint:nilerr // protocol validation errors are returned in-band
	}
	if existing, ok := r.commands[identity.RequestID]; ok {
		if existing.canonical != canonical {
			return acpClientFrames{client: rpcErrorFrame(id, -32602, "persistent interaction command changed", nil)}, nil
		}
		return acpClientFrames{client: rpcResultFrame(id, map[string]string{"eventId": existing.eventID})}, nil
	}
	request, pending := r.serverRequest(identity.RequestID)
	if !pending {
		return acpClientFrames{client: rpcErrorFrame(id, -32602, "persistent interaction is not pending", nil)}, nil
	}
	if err := ctx.Err(); err != nil {
		return acpClientFrames{}, err
	}
	command := acpInteractionCommand{
		eventID: r.newEventID(), canonical: canonical,
		params:      append(json.RawMessage(nil), params...),
		promptOwned: request.promptOwned,
	}
	r.commands[identity.RequestID] = command
	r.commandOrder = append(r.commandOrder, identity.RequestID)
	if command.promptOwned {
		r.promptReplay = append(r.promptReplay, acpReplayEntry{kind: acpReplayCommand, key: identity.RequestID})
	}
	return acpClientFrames{client: rpcResultFrame(id, map[string]string{"eventId": command.eventID})}, nil
}

func (r *acpRelay) clientCancellation(
	envelope map[string]json.RawMessage,
	fallback []byte,
	generation uint64,
) []byte {
	var params map[string]json.RawMessage
	if json.Unmarshal(envelope["params"], &params) != nil {
		return fallback
	}
	target := params["requestId"]
	for providerID, request := range r.pending {
		if request.generation != generation || string(request.clientID) != string(target) {
			continue
		}
		params["requestId"] = json.RawMessage(providerID)
		encoded, _ := json.Marshal(params)
		envelope["params"] = encoded
		return marshalFrame(envelope, fallback)
	}
	return fallback
}

// providerFrame returns the daemon-facing frame and whether the ACP profile owns
// its replay. A retained frame must not also enter the generic detached buffer or
// it would be replayed twice.
func (r *acpRelay) providerFrame(
	ctx context.Context,
	frame []byte,
	generation uint64,
	attached bool,
) ([]byte, bool, error) {
	// Capture the quiet window before the stamp below resets it. When a late
	// answer to a cancelled prompt arrives, this is the silence the watchdog is
	// deciding about, and it goes into the structured log.
	silence := time.Since(r.lastActivityAt)
	r.lastActivityAt = time.Now()
	var envelope map[string]json.RawMessage
	if json.Unmarshal(frame, &envelope) != nil {
		return frame, false, nil //nolint:nilerr // preserve malformed output for the SDK's protocol error
	}
	var method string
	_ = json.Unmarshal(envelope["method"], &method)
	id := envelope["id"]

	if method != "" {
		if len(id) > 0 && string(id) != "null" {
			rewritten, key, created := r.providerRequest(envelope, frame)
			if created && r.state.ActivePrompt {
				r.promptReplay = append(r.promptReplay, acpReplayEntry{kind: acpReplayRequest, key: key})
			}
			return rewritten, true, nil
		}
		r.captureAvailableCommands(method, envelope["params"])
		r.trackToolCall(method, envelope["params"])
		if r.state.ActivePrompt {
			eventID := r.newEventID()
			injectMeta(envelope, ACPEventIDMetaKey, eventID)
			rewritten := marshalFrame(envelope, frame)
			span, err := r.promptJournal.append(ctx, rewritten)
			if err != nil {
				return nil, false, err
			}
			r.promptReplay = append(r.promptReplay, acpReplayEntry{kind: acpReplayFrame, span: span})
			if !attached {
				return nil, true, nil
			}
			return rewritten, true, nil
		}
		return frame, false, nil
	}
	if len(id) == 0 {
		return frame, false, nil
	}
	if abandoned, ok := r.abandonedPrompts[string(id)]; ok {
		delete(r.abandonedPrompts, string(id))
		// A failure already went to the controller; the adapter's late answer
		// cannot replace it. Log the discard using the recorded stall window
		// and the real time since settlement, not the adapter's current delay.
		r.logStallDecision("late_result_discarded", r.state.SessionID, string(id),
			abandoned.silence, "settled_ago", time.Since(abandoned.settledAt).String(),
			"reason", "prompt already settled")
		return nil, false, nil
	}

	request, ok := r.pending[string(id)]
	if !ok {
		return frame, false, nil
	}
	delete(r.pending, string(id))
	r.captureResponse(request, envelope)

	if request.method == "session/prompt" {
		wasCancelled := !r.stallCancelledAt.IsZero() && r.stallCancelledPromptID == string(id)
		if wasCancelled {
			r.stallCancelledAt = time.Time{}
			r.stallCancelledPromptID = ""
			if isAdoptableStallAnswer(envelope) {
				// The adapter finished the turn normally after the watchdog
				// cancelled it (e.g. it produced the full answer just before
				// seeing the cancel). Adopt that success instead of replacing a
				// finished turn with a failure frame (the false-positive bug).
				r.logStallDecision("late_result_adopted", r.state.SessionID, string(id),
					silence, "action", "adopt successful result")
			} else {
				// A late error, or the adapter's plain acknowledgement of the
				// cancel (stopReason:cancelled), is not a completed turn. Keep
				// it a stall failure with the marker so the controller releases
				// its queue (issue #4861) rather than holding it on Interrupted.
				failure := map[string]json.RawMessage{}
				_ = json.Unmarshal(stalledPromptFailure(id, r.promptStallTimeout), &failure)
				envelope = failure
				r.logStallDecision("late_result_failure", r.state.SessionID, string(id),
					silence, "action", "forward stall failure")
			}
		}
		r.state.ActivePrompt = false
		r.state.ActiveCompaction = false
		r.cancelRequested = false
		r.clearToolCalls()
		eventID := r.newEventID()
		injectResultMeta(envelope, ACPEventIDMetaKey, eventID)
		rewritten := rewriteResponseID(envelope, request.clientID, frame)
		r.promptResult = promptResultNotification(envelope, eventID)
		r.state.PendingResultEventID = eventID
		if !attached {
			return nil, true, nil
		}
		if request.generation != generation {
			return append([]byte(nil), r.promptResult...), true, nil
		}
		return rewritten, true, nil
	}
	if request.generation != generation || !attached {
		return nil, false, nil
	}
	return rewriteResponseID(envelope, request.clientID, frame), false, nil
}

// failStalledPrompt fails an in-flight session/prompt that has heard nothing
// from the adapter for promptStallTimeout. It gives the turn a terminal state
// instead of leaving the session's Working indicator hanging until the next
// controller restart. A turn waiting on the client (a permission prompt or an
// elicitation) is not silent; the daemon owns that wait.
//
// It works in two steps. The first sends the adapter a session/cancel and keeps
// the prompt active: the adapter still owns the turn, and anything it emits
// until it answers belongs to that turn, not to the prompt the controller sends
// next. When the adapter answers, providerFrame turns its answer into the stall
// failure, which marks the adapter quiet and lets the controller release its
// queue. If it has not answered after promptStallCancelGrace it is wedged, and
// the second step settles the prompt without it and without the marker, so the
// queue stays held: the adapter may still emit from the old turn. Its late
// answer is dropped.
//
// The prompt may belong to an earlier controller generation: providerFrame then
// turns the failure into the private prompt result, which the reattached
// controller finishes against the turn it adopted. The returned client frame is
// nil while detached; replay delivers it on attach. Callers must hold the host
// mutex.
func (r *acpRelay) failStalledPrompt(
	ctx context.Context,
	generation uint64,
	attached bool,
	now time.Time,
) (acpClientFrames, error) {
	if !r.state.ActivePrompt {
		return acpClientFrames{}, nil
	}
	cancelling := !r.stallCancelledAt.IsZero()
	silence := now.Sub(r.lastActivityAt)
	// A turn waiting on an adapter request to the client (permission,
	// elicitation) is not silent: the daemon owns that wait. Otherwise the
	// deadline is the base budget, or the longer tool-call cap while the
	// adapter has a live tool call.
	if !cancelling && (len(r.serverRequests) > 0 || silence < r.stallTimeout()) {
		return acpClientFrames{}, nil
	}
	if cancelling && now.Sub(r.stallCancelledAt) < promptStallCancelGrace {
		return acpClientFrames{}, nil
	}
	for providerID, request := range r.pending {
		if request.method != "session/prompt" {
			continue
		}
		if !cancelling {
			r.stallCancelledAt = now
			r.stallCancelledPromptID = providerID
			var prompt struct {
				SessionID string `json:"sessionId"`
			}
			_ = json.Unmarshal(request.params, &prompt)
			params, _ := json.Marshal(prompt)
			cancel := marshalFrame(map[string]json.RawMessage{
				"jsonrpc": json.RawMessage(`"2.0"`), "method": json.RawMessage(`"session/cancel"`), "params": params,
			}, nil)
			r.logStallDecision("stall_cancel_sent", prompt.SessionID, providerID, silence,
				"deadline", r.stallTimeout().String())
			return acpClientFrames{provider: cancel}, nil
		}
		// The adapter ignored the cancel. Settle without the stall marker: the
		// adapter may still emit from the old turn, so the controller must hold
		// its queue like any other failure rather than start a prompt on it.
		r.stallCancelledAt = time.Time{}
		r.stallCancelledPromptID = ""
		failure := rpcErrorFrame(json.RawMessage(providerID), -32603, fmt.Sprintf(
			"provider stopped responding: no ACP output for %s and no answer to the cancel; failing the stalled prompt",
			r.promptStallTimeout), nil)
		completion, _, err := r.providerFrame(ctx, failure, generation, attached)
		if err != nil {
			return acpClientFrames{}, err
		}
		r.abandonedPrompts[providerID] = abandonedPrompt{settledAt: now, silence: silence}
		r.logStallDecision("stall_failure_emitted", r.state.SessionID, providerID, silence,
			"reason", "cancel grace expired")
		return acpClientFrames{client: completion}, nil
	}
	return acpClientFrames{}, nil
}

// logStallDecision records a watchdog action. Fields are stable so operators
// can correlate a cancelled prompt, its silence window, and whether its late
// answer was adopted or discarded.
func (r *acpRelay) logStallDecision(event, sessionID, promptID string, silence time.Duration, extra ...any) {
	if r.logger == nil {
		return
	}
	attrs := make([]any, 0, 6+len(extra))
	attrs = append(attrs,
		"session_id", sessionID,
		"prompt_id", promptID,
		"silent_for", silence.String(),
	)
	attrs = append(attrs, extra...)
	r.logger.Info(event, attrs...)
}

func (r *acpRelay) clearToolCalls() {
	for id := range r.activeToolCalls {
		delete(r.activeToolCalls, id)
	}
}

// isAdoptableStallAnswer reports whether a late answer to a watchdog-cancelled
// prompt is a genuinely completed turn whose success should be adopted. Only a
// successful result whose stopReason is end_turn qualifies: a plain
// stopReason:cancelled is the adapter's acknowledgement of session/cancel and
// must stay a stall failure, as must any error result.
func isAdoptableStallAnswer(envelope map[string]json.RawMessage) bool {
	if raw, ok := envelope["error"]; ok && len(raw) > 0 && string(raw) != "null" {
		return false
	}
	raw, ok := envelope["result"]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var result struct {
		StopReason string `json:"stopReason"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return false
	}
	return result.StopReason == "end_turn"
}

// stalledPromptFailure is the error a prompt ends with when the watchdog
// confirmed a failure after cancelling it for silence. The data marker lets the
// daemon tell it from a provider that answered with an error.
func stalledPromptFailure(id json.RawMessage, timeout time.Duration) []byte {
	return rpcErrorFrame(id, -32603, fmt.Sprintf(
		"provider stopped responding: no ACP output for %s; failing the stalled prompt", timeout),
		map[string]any{ACPPromptStalledDataKey: true})
}

func (r *acpRelay) providerRequest(
	envelope map[string]json.RawMessage,
	fallback []byte,
) (frame []byte, key string, created bool) {
	id := envelope["id"]
	key = string(id)
	request := r.serverRequests[key]
	if request.requestID == "" {
		r.nextInteractionID++
		request.requestID = fmt.Sprintf("acp-request:%s:%d", r.identity, r.nextInteractionID)
		request.promptOwned = r.state.ActivePrompt
		r.serverOrder = append(r.serverOrder, key)
		created = true
	}
	injectMeta(envelope, ACPRequestIDMetaKey, request.requestID)
	request.frame = marshalFrame(envelope, fallback)
	r.serverRequests[key] = request
	return request.frame, request.requestID, created
}

func (r *acpRelay) serverRequest(requestID string) (acpServerRequest, bool) {
	for _, request := range r.serverRequests {
		if request.requestID == requestID {
			return request, true
		}
	}
	return acpServerRequest{}, false
}

func (r *acpRelay) removeServerRequest(providerID string) {
	if _, ok := r.serverRequests[providerID]; !ok {
		return
	}
	delete(r.serverRequests, providerID)
	for i, id := range r.serverOrder {
		if id == providerID {
			r.serverOrder = append(r.serverOrder[:i], r.serverOrder[i+1:]...)
			return
		}
	}
}

func canonicalJSON(raw json.RawMessage) (string, error) {
	if !json.Valid(raw) {
		return "", errors.New("invalid JSON")
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(value)
	return string(encoded), err
}

func rpcResultFrame(id json.RawMessage, result any) []byte {
	encoded, _ := json.Marshal(result)
	return marshalFrame(map[string]json.RawMessage{
		"jsonrpc": json.RawMessage(`"2.0"`), "id": id, "result": encoded,
	}, nil)
}

func rpcErrorFrame(id json.RawMessage, code int, message string, data map[string]any) []byte {
	body := map[string]any{"code": code, "message": message}
	if data != nil {
		body["data"] = data
	}
	encoded, _ := json.Marshal(body)
	return marshalFrame(map[string]json.RawMessage{
		"jsonrpc": json.RawMessage(`"2.0"`), "id": id, "error": encoded,
	}, nil)
}

func (r *acpRelay) captureResponse(request acpClientRequest, envelope map[string]json.RawMessage) {
	if len(envelope["error"]) > 0 && string(envelope["error"]) != "null" {
		return
	}
	result := envelope["result"]
	switch request.method {
	case "initialize":
		var params struct {
			Meta map[string]string `json:"_meta"`
		}
		_ = json.Unmarshal(request.params, &params)
		r.state.InitialPermissions = params.Meta[ACPInitialPermissionsMetaKey]
		r.state.InitializeResult = append(json.RawMessage(nil), result...)
	case "session/new", "session/load", "session/resume", "session/fork":
		var response struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(result, &response)
		if request.method == "session/fork" {
			var params struct {
				SessionID string `json:"sessionId"`
			}
			_ = json.Unmarshal(request.params, &params)
			if response.SessionID == "" || response.SessionID == params.SessionID {
				return
			}
		}
		r.state.SessionResult = append(json.RawMessage(nil), result...)
		if response.SessionID != "" {
			r.state.SessionID = response.SessionID
		} else if request.method != "session/fork" {
			var params struct {
				SessionID string `json:"sessionId"`
			}
			_ = json.Unmarshal(request.params, &params)
			if params.SessionID != "" {
				r.state.SessionID = params.SessionID
			}
		}
	}
}

func (r *acpRelay) newEventID() string {
	r.nextEventID++
	return fmt.Sprintf("acp-host:%s:%d", r.identity, r.nextEventID)
}

func rewriteResponseID(envelope map[string]json.RawMessage, id json.RawMessage, fallback []byte) []byte {
	envelope["id"] = append(json.RawMessage(nil), id...)
	return marshalFrame(envelope, fallback)
}

func promptResultNotification(response map[string]json.RawMessage, eventID string) []byte {
	params := map[string]json.RawMessage{}
	if result := response["result"]; len(result) > 0 {
		params["result"] = result
	}
	if rpcErr := response["error"]; len(rpcErr) > 0 {
		params["error"] = rpcErr
	}
	eventJSON, _ := json.Marshal(eventID)
	params["eventId"] = eventJSON
	encodedParams, _ := json.Marshal(params)
	method, _ := json.Marshal(ACPPromptResultMethod)
	return marshalFrame(map[string]json.RawMessage{
		"jsonrpc": json.RawMessage(`"2.0"`), "method": method, "params": encodedParams,
	}, nil)
}

func injectResultMeta(envelope map[string]json.RawMessage, key, value string) {
	var rpcErr map[string]json.RawMessage
	if json.Unmarshal(envelope["error"], &rpcErr) == nil && rpcErr != nil {
		// Error data can be any JSON value. Wrap it rather than overwrite it.
		data := map[string]json.RawMessage{"providerData": rpcErr["data"]}
		data[key], _ = json.Marshal(value)
		if len(data["providerData"]) == 0 {
			delete(data, "providerData")
		}
		rpcErr["data"], _ = json.Marshal(data)
		envelope["error"], _ = json.Marshal(rpcErr)
		return
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(envelope["result"], &result) != nil {
		return
	}
	if result == nil {
		result = make(map[string]json.RawMessage)
	}
	injectRawMeta(result, key, value)
	encoded, _ := json.Marshal(result)
	envelope["result"] = encoded
}

func injectMeta(envelope map[string]json.RawMessage, key, value string) {
	var params map[string]json.RawMessage
	if json.Unmarshal(envelope["params"], &params) != nil || params == nil {
		params = make(map[string]json.RawMessage)
	}
	injectRawMeta(params, key, value)
	encoded, _ := json.Marshal(params)
	envelope["params"] = encoded
}

func injectRawMeta(object map[string]json.RawMessage, key, value string) {
	var meta map[string]json.RawMessage
	if json.Unmarshal(object["_meta"], &meta) != nil || meta == nil {
		meta = make(map[string]json.RawMessage)
	}
	encoded, _ := json.Marshal(value)
	meta[key] = encoded
	encodedMeta, _ := json.Marshal(meta)
	object["_meta"] = encodedMeta
}

func marshalFrame(envelope map[string]json.RawMessage, fallback []byte) []byte {
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return fallback
	}
	return append(encoded, '\n')
}

func isCompactionPrompt(params json.RawMessage) bool {
	if len(params) == 0 {
		return false
	}
	var req struct {
		Prompt []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return false
	}
	for _, block := range req.Prompt {
		if block.Type == "text" && strings.TrimSpace(block.Text) == "/compact" {
			return true
		}
	}
	return false
}
